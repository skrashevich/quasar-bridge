#!/usr/bin/env python3
"""Provision an isolated real kuyase Station 2 VM for VHS installer recordings."""

import argparse
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import socket
import subprocess
import time


ADB_PORT = 25555
BRIDGE_PORT = 28091
SSH_PORT = 2222
GUEST = "/data/vhs-ssh"


def run(command, **kwargs):
    return subprocess.check_output(command, text=True, stderr=subprocess.STDOUT,
                                   timeout=60, **kwargs).strip()


def adb_shell(command):
    wrapped = f"( {command} ); rc=$?; printf '\\n__VHS_RC=%s\\n' \"$rc\""
    output = run(["adb", "-s", f"127.0.0.1:{ADB_PORT}", "shell", wrapped]).replace("\r", "")
    match = re.search(r"(?:^|\n)__VHS_RC=(\d+)\s*$", output)
    if not match or match[1] != "0":
        raise RuntimeError(f"Guest command failed: {command}\n{output}")
    return output[:match.start()].strip()


def push(source, target):
    run(["adb", "-s", f"127.0.0.1:{ADB_PORT}", "push", str(source), target])


def launch(command, log, cwd=None):
    with log.open("wb") as output:
        process = subprocess.Popen(command, cwd=cwd, stdin=subprocess.DEVNULL,
                                   stdout=output, stderr=subprocess.STDOUT,
                                   start_new_session=True)
    return process


def stop(workspace):
    state_path = workspace / "session.json"
    if not state_path.exists():
        print("No owned session")
        return
    state = json.loads(state_path.read_text())

    def members():
        groups = {}
        output = run(["ps", "-A", "-o", "pid=,pgid=,stat=,command="])
        for line in output.splitlines():
            fields = line.split(None, 3)
            if len(fields) == 4 and not fields[2].startswith("Z"):
                groups.setdefault(int(fields[1]), []).append(fields[3])
        return groups

    groups = {state[key]: marker for key, marker in
              (("dropbear_pid", f"127.0.0.1:{ADB_PORT}"),
               ("vm_pid", str(workspace))) if state.get(key)}

    def signal_owned(number):
        live = members()
        for pgid, marker in groups.items():
            commands = live.get(pgid, [])
            if not commands:
                continue
            if not any(marker in command for command in commands):
                raise RuntimeError(f"Cannot verify ownership of process group {pgid}; session retained")
            try:
                os.killpg(pgid, number)
            except ProcessLookupError:
                pass

    def wait_stopped(timeout):
        deadline = time.monotonic() + timeout
        while any(pgid in members() for pgid in groups):
            if time.monotonic() >= deadline:
                return False
            time.sleep(.1)
        return True

    signal_owned(signal.SIGTERM)
    if not wait_stopped(5):
        signal_owned(signal.SIGKILL)
        if not wait_stopped(5):
            raise RuntimeError("Owned processes are still running; session retained")
    state_path.unlink()
    print("Stopped owned kuyase session")


def start(args):
    workspace = args.workspace.resolve()
    source = args.emulator_dir.resolve()
    dropbear = args.dropbear_dir.resolve()
    workspace.mkdir(parents=True, exist_ok=True)
    if (workspace / "session.json").exists():
        raise RuntimeError("Session already exists; stop it before starting a fresh VM")
    for port in (ADB_PORT, BRIDGE_PORT, SSH_PORT):
        with socket.socket() as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(("127.0.0.1", port))
    binary = source / "target/release/kuyase"
    if not binary.is_file():
        binary = source / "kuyase"
    inputs = {
        binary: workspace / "kuyase",
        source / "kuyase-kernel-arm64.img": workspace / "kuyase-kernel-arm64.img",
        source / "guest-init/bin/qemu-hw-bridge": workspace / "guest-init/bin/qemu-hw-bridge",
        source / "work/station2-system.ext4": workspace / "work/station2-system.ext4",
    }
    for src, dst in inputs.items():
        if not src.is_file():
            raise FileNotFoundError(src)
        dst.parent.mkdir(parents=True, exist_ok=True)
        # Separate copies prevent kuyase staging or disk writes touching source assets.
        shutil.copy2(src, dst)
    for name in ("dropbear", "dropbearkey"):
        if not (dropbear / name).is_file():
            raise FileNotFoundError(dropbear / name)
    qemu = shutil.which("qemu-system-aarch64")
    if not qemu:
        raise RuntimeError("qemu-system-aarch64 is required")
    wrapper = workspace / "qemu-system-aarch64"
    wrapper.write_text(
        "#!/usr/bin/env python3\nimport os, sys\n"
        "args = sys.argv[1:]\n"
        "for index, value in enumerate(args):\n"
        "    if value == '-netdev' and args[index+1].startswith('user,id=net0,'):\n"
        f"        args[index+1] += ',hostfwd=tcp:127.0.0.1:{SSH_PORT}-:22'\n"
        f"os.execv({qemu!r}, [{qemu!r}, *args])\n")
    wrapper.chmod(0o755)
    key = workspace / "id_ed25519"
    if not key.exists():
        run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "vhs-kuyase", "-f", str(key)])
    config = workspace / "ssh_config"
    config.write_text(
        "Host 127.0.0.1 kuyase-station2\n"
        "  HostName 127.0.0.1\n  User root\n"
        f"  Port {SSH_PORT}\n  IdentityFile {key}\n"
        f"  UserKnownHostsFile {workspace / 'known_hosts'}\n"
        "  StrictHostKeyChecking accept-new\n  IdentitiesOnly yes\n"
        "  BatchMode yes\n  ConnectTimeout 10\n  LogLevel ERROR\n")
    config.chmod(0o600)
    known_hosts = workspace / "known_hosts"
    if known_hosts.exists():
        known_hosts.unlink()  # The fresh snapshot receives a new ephemeral host key.
    state = {}
    try:
        vm = launch([str(workspace / "kuyase"), "run", "--headless", "--board", "station2",
                     "--mode", "stub", "--snapshot", "--no-persist-data", "--keep-alive",
                     "--adb-port", str(ADB_PORT), "--bridge-port", str(BRIDGE_PORT)],
                    workspace / "vm.log", cwd=workspace)
        state["vm_pid"] = vm.pid
        (workspace / "session.json").write_text(json.dumps(state) + "\n")
        deadline = time.monotonic() + 120
        while "Headless VM 'station2' READY" not in (workspace / "vm.log").read_text(errors="replace"):
            if vm.poll() is not None:
                raise RuntimeError((workspace / "vm.log").read_text())
            if time.monotonic() > deadline:
                raise TimeoutError("VM readiness timeout; see vm.log and work/station2-serial.log")
            time.sleep(.5)
        run(["adb", "connect", f"127.0.0.1:{ADB_PORT}"])
        if adb_shell("hostname") != "qemu-station2":
            raise RuntimeError("Unexpected guest hostname")
        adb_shell(f"mkdir -p {GUEST} /root/.ssh; chmod 700 {GUEST} /root/.ssh")
        for name in ("dropbear", "dropbearkey"):
            push(dropbear / name, f"{GUEST}/{name}")
        push(key.with_suffix(".pub"), "/root/.ssh/authorized_keys")
        adb_shell(f"chmod 600 /root/.ssh/authorized_keys; chmod 755 {GUEST}/dropbear*; "
                  f"{GUEST}/dropbearkey -t ed25519 -f {GUEST}/hostkey >/dev/null")
        transport = launch(["adb", "-s", f"127.0.0.1:{ADB_PORT}", "shell",
                            f"exec {GUEST}/dropbear -F -s -g -p 22 -r {GUEST}/hostkey -P {GUEST}/dropbear.pid"],
                           workspace / "dropbear.log")
        state["dropbear_pid"] = transport.pid
        (workspace / "session.json").write_text(json.dumps(state) + "\n")
        deadline = time.monotonic() + 20
        while True:
            try:
                output = run(["ssh", "-F", str(config), "root@127.0.0.1", "hostname; uname -m; id -u"])
                if output.splitlines() != ["qemu-station2", "aarch64", "0"]:
                    raise RuntimeError(f"Unexpected SSH guest: {output}")
                break
            except subprocess.CalledProcessError:
                if transport.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError((workspace / "dropbear.log").read_text())
                time.sleep(.5)
        print(output)
        print(f"SSH ready: ssh -F {shlex.quote(str(config))} root@127.0.0.1")
        print("Real installer may now use root@127.0.0.1 -F " + shlex.quote(str(config)))
    except BaseException:
        stop(workspace)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("start", "stop", "status"))
    parser.add_argument("--workspace", type=Path, default=Path("runtime/vhs-kuyase"))
    parser.add_argument("--emulator-dir", type=Path)
    parser.add_argument("--dropbear-dir", type=Path)
    args = parser.parse_args()
    if args.action == "start":
        if args.emulator_dir is None or args.dropbear_dir is None:
            parser.error("start requires --emulator-dir and --dropbear-dir")
        start(args)
    elif args.action == "stop":
        stop(args.workspace.resolve())
    else:
        print(run(["ssh", "-F", str(args.workspace.resolve() / "ssh_config"),
                   "root@127.0.0.1", "hostname; uname -m; id -u"]))


if __name__ == "__main__":
    main()
