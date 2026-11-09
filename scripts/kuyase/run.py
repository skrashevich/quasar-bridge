#!/usr/bin/env python3
"""Test real standalone Bridge builds in an ephemeral kuyase station VM."""
import argparse
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import tarfile
import time


ADB = ["adb", "-s", "127.0.0.1:15555"]
GUEST = "/data/bridge-ci"
QUASAR = "/system/vendor/quasar"


def shell(command, timeout=60):
    # Old station adbd does not propagate shell exit codes to the host.
    wrapped = "( " + command + " ); rc=$?; printf '\\n__BRIDGE_RC=%s\\n' \"$rc\""
    output = subprocess.check_output(ADB + ["shell", wrapped], timeout=timeout)
    output = output.decode(errors="replace").replace("\r", "")
    match = re.search(r"\n__BRIDGE_RC=(\d+)\s*$", output)
    if not match or int(match[1]):
        raise RuntimeError(f"guest command failed: {command}\n{output}")
    return output[:match.start()].strip()


def push(source, target):
    subprocess.run(ADB + ["push", str(source), target], check=True, timeout=90)


def health():
    result = json.loads(shell("wget -q -T 4 -O - http://127.0.0.1:8443/healthz"))
    if result.get("status") != "ok":
        raise RuntimeError(f"unhealthy Bridge: {result}")
    return result


def stop(process):
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def check_second_wake_word(out, arch):
    """Enable and roll back the second wake word on the firmware's own files.

    The VM runs the public OTA, so this checks the real quasar.cfg layout, the
    shipped spotter models and the voice daemon of this board. It cannot check
    recognition: the emulator has no microphone.
    """
    config = f"{QUASAR}/quasar.cfg"
    tool = (f"{GUEST}/quasar-bridge -speaker-config {config} "
            f"-speaker-wake-state {GUEST}/wake-{arch}")
    status = shell(f"{tool} -speaker-wake status")
    (out / f"{arch}-wake-status.txt").write_text(status + "\n")
    if "firmware support: yes" not in status or "customActivation absent" not in status:
        raise RuntimeError(f"unexpected second wake word status:\n{status}")
    before = shell(f"sha256sum {config}").split()[0]
    fs_info = shell(f"ls -la {QUASAR}/quasar.cfg; mount; id")
    (out / f"{arch}-wake-fs.txt").write_text(fs_info + "\n")
    print(fs_info, flush=True)
    # Same step as the installer's firmware.sh; a snapshot VM may already be writable.
    shell(f"touch {QUASAR}/.bridge-ci && rm {QUASAR}/.bridge-ci || mount -o remount,rw /")
    for word in ("yandex", "alisa"):
        shell(f"{tool} -speaker-wake apply -speaker-wake-word {word}")
        patched = json.loads(shell(f"cat {config}"))
        if patched["aliced"].get("customActivation") != {"forceEnable": True, "forcePhrase": word}:
            raise RuntimeError(f"customActivation missing after enabling {word}")
        listing = "find . -type f ! -name .second-wake-word | sort"
        expected = shell(f"cd {QUASAR}/spotter_models/activation/{word} && {listing}")
        actual = shell(f"cd {QUASAR}/spotter_models/custom_activation && {listing}")
        if not expected or expected != actual:
            raise RuntimeError(f"second slot does not match the {word} model:\n{expected}\n---\n{actual}")
        shell(f"{tool} -speaker-wake apply -speaker-wake-word {word}; test $? -ne 0")  # already enabled
        shell(f"{tool} -speaker-wake rollback")
        if shell(f"sha256sum {config}").split()[0] != before:
            raise RuntimeError(f"quasar.cfg differs after rolling back {word}")
        shell(f"test ! -e {QUASAR}/spotter_models/custom_activation")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--board", choices=("yandexmini", "station2"), required=True)
    parser.add_argument("--emulator-dir", type=Path, required=True)
    parser.add_argument("--build-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    emulator, build, out = (p.resolve() for p in
                            (args.emulator_dir, args.build_dir, args.output))
    out.mkdir(parents=True, exist_ok=True)
    # Refuse to share ADB/bridge ports with an existing emulator session.
    for port in (15555, 18090, 18091):
        with socket.socket() as listener:
            listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            listener.bind(("127.0.0.1", port))
    result = {"board": args.board, "builds": [], "result": "FAIL"}
    processes, logs = [], []

    def launch(command, name, cwd=None, group=False):
        log = (out / name).open("wb")
        logs.append(log)
        proc = subprocess.Popen(command, cwd=cwd, stdin=subprocess.PIPE,
                                stdout=log, stderr=subprocess.STDOUT,
                                start_new_session=group)
        processes.append(proc)
        return proc

    vm = launch([str(emulator / "kuyase"), "run", "--headless", "--board", args.board,
                 "--mode", "stub", "--snapshot", "--keep-alive"],
                "vm.log", cwd=emulator, group=True)
    try:
        deadline = time.monotonic() + 180
        while f"Headless VM '{args.board}' READY" not in (out / "vm.log").read_text(errors="replace"):
            if vm.poll() is not None:
                raise RuntimeError("VM exited; see vm.log")
            if time.monotonic() >= deadline:
                raise TimeoutError("VM readiness timed out; see vm.log")
            time.sleep(1)
        subprocess.run(["adb", "connect", "127.0.0.1:15555"], check=True, timeout=15)
        if shell("hostname") != "qemu-" + args.board:
            raise RuntimeError("connected to wrong station VM")
        (out / "guest.txt").write_text(shell("uname -a; cat /tmp/quasar-emu-hw.json"))
        shell(f"mkdir -p {GUEST}")
        push(build / "kuyase-control", GUEST + "/control")
        shell(f"chmod 755 {GUEST}/control")
        # Keep ADB shell transports open: legacy adbd kills detached children.
        launch(ADB + ["shell", f"exec {GUEST}/control -mode upstream >{GUEST}/mock.log 2>&1"],
               "mock-transport.log")
        for arch in ("arm64", "armv7"):
            with tarfile.open(build / f"quasar-bridge-linux-{arch}.tar.gz") as archive:
                binary = out / "quasar-bridge"
                binary.write_bytes(archive.extractfile("bin/quasar-bridge").read())
            push(binary, GUEST + "/quasar-bridge")
            binary.unlink()
            config = {
                "listen": "127.0.0.1:8443", "upstream": "ws://127.0.0.1:18444/uni.ws",
                "mode": "hybrid", "llm_base_url": "http://127.0.0.1:18444/v1",
                "llm_model": "unused-local-fixture", "assistant_tools": {"enabled": False},
                "dialogue_context": {"enabled": False}, "conversation_log": {"enabled": False},
                "local_progress": {"enabled": False}, "listen_after_reply": False,
                "airplay": {"interface": "eth0", "listen_address": ":7000",
                            "data_dir": f"{GUEST}/state-{arch}", "device": "default"},
            }
            config_file = out / f"{arch}-config.json"
            config_file.write_text(json.dumps(config, indent=2) + "\n")
            push(config_file, GUEST + "/config.json")
            shell(f"chmod 755 {GUEST}/quasar-bridge; {GUEST}/quasar-bridge -config {GUEST}/config.json -check-config")
            check_second_wake_word(out, arch)
            command = ("export GOMAXPROCS=2; "
                       f"echo $$ >{GUEST}/bridge.pid; exec {GUEST}/quasar-bridge "
                       f"-config {GUEST}/config.json >{GUEST}/bridge.log 2>&1")
            bridge = launch(ADB + ["shell", command], f"{arch}-transport.log")
            deadline = time.monotonic() + 60
            while True:
                try:
                    before = health()
                    break
                except (RuntimeError, ValueError, subprocess.SubprocessError):
                    if bridge.poll() is not None or time.monotonic() >= deadline:
                        raise RuntimeError("Bridge startup failed; see failure-bridge.log")
                    time.sleep(1)
            if before["airplay"]["state"] != "stopped":
                raise RuntimeError(f"unexpected initial AirPlay state: {before}")
            for action in ("start", "stop"):
                output = shell(f"{GUEST}/control -action {action} -timeout 60s", timeout=75)
                (out / f"{arch}-{action}.txt").write_text(output + "\n")
            after = health()
            if after["airplay"]["state"] != "stopped" or shell("pidof station-pcm || true"):
                raise RuntimeError("AirPlay did not shut down cleanly")
            shell(f"kill $(cat {GUEST}/bridge.pid)")
            bridge.wait(timeout=15)
            # Signal exit codes vary between old adbd versions; the PID must be gone.
            shell(f"! kill -0 $(cat {GUEST}/bridge.pid) 2>/dev/null")
            (out / f"{arch}-bridge.log").write_text(shell(f"cat {GUEST}/bridge.log"))
            result["builds"].append({"arch": arch, "result": "PASS"})
            print(f"PASS {args.board} {arch}: config, health, VoiceInput/ASR, AirPlay start/stop, second wake word", flush=True)
        mock_log = shell(f"cat {GUEST}/mock.log")
        (out / "mock.log").write_text(mock_log)
        if "UNEXPECTED" in mock_log or "FAIL" in mock_log:
            raise RuntimeError("unexpected cloud request; see mock.log")
        result["result"] = "PASS"
    except Exception as error:
        result["error"] = str(error)
        for name in ("bridge", "mock"):
            try:
                (out / f"failure-{name}.log").write_text(shell(f"cat {GUEST}/{name}.log", timeout=10))
            except Exception:
                pass
        raise
    finally:
        (out / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        for process in reversed(processes[1:]):
            stop(process)
        # kuyase owns QEMU; terminate only the process group created above.
        try:
            os.killpg(vm.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        stop(vm)
        # kuyase redirects QEMU stderr/kernel output to its own serial log.
        # The runner's vm.log otherwise only reports a bridge connection error.
        serial_log = emulator / "work" / f"{args.board}-serial.log"
        if serial_log.is_file():
            serial = serial_log.read_text(errors="replace")
            (out / "qemu-serial.log").write_text(serial)
            if result["result"] != "PASS":
                print(serial, end="", flush=True)
        for log in logs:
            log.close()


if __name__ == "__main__":
    main()
