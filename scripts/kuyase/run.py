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

from report import Report


ADB = ["adb", "-s", "127.0.0.1:15555"]
GUEST = "/data/bridge-ci"


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


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--board", choices=("yandexmini", "station2"), required=True)
    parser.add_argument("--arch", choices=("arm64", "armv7"), help="Test one package (default: both)")
    parser.add_argument("--emulator-dir", type=Path, required=True)
    parser.add_argument("--build-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    emulator, build, out = (p.resolve() for p in
                            (args.emulator_dir, args.build_dir, args.output))
    out.mkdir(parents=True, exist_ok=True)
    report = Report(args.board, out)
    result = {"result": "FAIL"}
    processes, logs = [], []

    def launch(command, name, cwd=None, group=False):
        log = (out / name).open("wb")
        logs.append(log)
        proc = subprocess.Popen(command, cwd=cwd, stdin=subprocess.PIPE,
                                stdout=log, stderr=subprocess.STDOUT,
                                start_new_session=group)
        processes.append(proc)
        return proc

    vm = None
    try:
        # Refuse to share ADB/bridge ports with an existing emulator session.
        for port in (15555, 18090, 18091):
            with socket.socket() as listener:
                listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                listener.bind(("127.0.0.1", port))
        vm = launch([str(emulator / "kuyase"), "run", "--headless", "--board", args.board,
                 "--mode", "stub", "--snapshot", "--keep-alive"],
                    "vm.log", cwd=emulator, group=True)
        deadline = time.monotonic() + 180
        while f"Headless VM '{args.board}' READY" not in (out / "vm.log").read_text(errors="replace"):
            if vm.poll() is not None:
                raise RuntimeError("VM exited; see vm.log")
            if time.monotonic() >= deadline:
                raise TimeoutError("VM readiness timed out; see vm.log")
            time.sleep(1)
        subprocess.run(["adb", "disconnect", "127.0.0.1:15555"], timeout=15)
        subprocess.run(["adb", "connect", "127.0.0.1:15555"], check=True, timeout=15)
        if shell("hostname") != "qemu-" + args.board:
            raise RuntimeError("connected to wrong station VM")
        (out / "guest.txt").write_text(shell("uname -a; cat /tmp/quasar-emu-hw.json"))
        shell(f"mkdir -p {GUEST}")
        push(build / "kuyase-control", GUEST + "/control")
        shell(f"chmod 755 {GUEST}/control")
        # Keep ADB shell transports open: legacy adbd kills detached children.
        for arch in ([args.arch] if args.arch else ("arm64", "armv7")):
            mock = launch(ADB + ["shell", f"echo $$ >{GUEST}/mock.pid; exec {GUEST}/control -mode upstream >{GUEST}/mock.log 2>&1"], f"{arch}-mock-transport.log")
            with tarfile.open(build / f"quasar-bridge-linux-{arch}.tar.gz") as archive:
                binary = out / "quasar-bridge"
                binary.write_bytes(archive.extractfile("bin/quasar-bridge").read())
            push(binary, GUEST + "/quasar-bridge")
            binary.unlink()
            config = {
                "listen": "127.0.0.1:8443", "upstream": "ws://127.0.0.1:18444/uni.ws",
                "mode": "hybrid", "llm_base_url": "http://127.0.0.1:18444/v1",
                "llm_model": "kuyase-fixture",
                "routing": {"default_route": "yandex", "classifier": {"enabled": False}}, "assistant_tools": {"enabled": False},
                "dialogue_context": {"enabled": True}, "conversation_log": {"enabled": False},
                "local_progress": {"enabled": False}, "listen_after_reply": False,
                "airplay": {"interface": "eth0", "listen_address": ":7000",
                            "data_dir": f"{GUEST}/state-{arch}", "device": "default"},
            }
            config_file = out / f"{arch}-config.json"
            config_file.write_text(json.dumps(config, indent=2) + "\n")
            push(config_file, GUEST + "/config.json")
            if not report.check(arch, "config", lambda: shell(f"chmod 755 {GUEST}/quasar-bridge; {GUEST}/quasar-bridge -config {GUEST}/config.json -check-config")):
                shell(f"kill $(cat {GUEST}/mock.pid)")
                mock.wait(timeout=15)
                continue
            command = ("export GOMAXPROCS=2 QUASAR_HOME_MCP_URL=http://127.0.0.1:18444/mcp; "
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
            report.check(arch, "health", health)
            for action in ("weather", "music", "timer", "llm", "text", "dialogue",
                           "isolation", "reset", "home", "failure", "start", "stop"):
                def check_action():
                    output = shell(f"{GUEST}/control -action {action} -timeout 90s", timeout=100)
                    (out / f"{arch}-{action}.txt").write_text(output + "\n")
                report.check(arch, action, check_action)

            def check_cleanup():
                after = health()
                if after["airplay"]["state"] != "stopped" or shell("pidof station-pcm || true"):
                    raise RuntimeError("AirPlay did not shut down cleanly")
            report.check(arch, "airplay-cleanup", check_cleanup)

            def check_shutdown():
                shell(f"kill $(cat {GUEST}/bridge.pid)")
                bridge.wait(timeout=15)
                shell(f"! kill -0 $(cat {GUEST}/bridge.pid) 2>/dev/null")
            if not report.check(arch, "shutdown", check_shutdown):
                raise RuntimeError("Bridge did not shut down; cannot reuse VM")
            (out / f"{arch}-bridge.log").write_text(shell(f"cat {GUEST}/bridge.log"))
            mock_log = shell(f"cat {GUEST}/mock.log")
            (out / f"{arch}-mock.log").write_text(mock_log)
            if "UNEXPECTED" in mock_log or "FAIL" in mock_log:
                raise RuntimeError("unexpected backend request; see mock.log")
            shell(f"kill $(cat {GUEST}/mock.pid)")
            mock.wait(timeout=15)
        mock_log = shell(f"cat {GUEST}/mock.log")
        (out / "mock.log").write_text(mock_log)
        if "UNEXPECTED" in mock_log or "FAIL" in mock_log:
            raise RuntimeError("unexpected cloud request; see mock.log")
        if any(case["result"] != "PASS" for case in report.cases):
            raise RuntimeError("E2E regressions detected; see result.json and junit.xml")
        result["result"] = "PASS"
    except Exception as error:
        if not any(case["result"] != "PASS" for case in report.cases):
            def failure():
                raise error
            report.check(args.arch or "all", "infrastructure", failure)
        for name in ("bridge", "mock"):
            try:
                (out / f"failure-{name}.log").write_text(shell(f"cat {GUEST}/{name}.log", timeout=10))
            except Exception:
                pass
        raise
    finally:
        for process in reversed(processes[1:]):
            stop(process)
        # kuyase owns QEMU; terminate only the process group created above.
        if vm is not None:
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
        report.summary()


if __name__ == "__main__":
    main()
