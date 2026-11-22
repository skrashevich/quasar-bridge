#!/usr/bin/env python3
"""Test real standalone Bridge builds in an ephemeral kuyase station VM."""
import argparse
from contextlib import contextmanager, suppress
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import tarfile
import tempfile
import time

from firmware import FIRMWARE
from report import Report


ADB = ["adb", "-s", "127.0.0.1:15555"]
GUEST = "/data/bridge-ci"


def shell(command, timeout=60):
    # Legacy adbd can close the shell stream before delivering its last bytes.
    # Publish output and exit status together, then fetch them via ADB file sync.
    with tempfile.TemporaryDirectory(prefix="quasar-ci-shell-") as tmp:
        local = Path(tmp) / "result"
        remote = f"/tmp/{Path(tmp).name}.result"
        wrapped = (f"{{ ( {command} ); printf '\\n__BRIDGE_RC=%s\\n' \"$?\"; }} "
                   f">{remote}.part 2>&1 && mv {remote}.part {remote}")
        try:
            subprocess.run(ADB + ["shell", wrapped], check=True, timeout=timeout)
            pull = subprocess.run(ADB + ["pull", remote, str(local)], capture_output=True, timeout=10)
            if pull.returncode:
                reason = (pull.stderr + pull.stdout).decode(errors="replace").strip()
                raise RuntimeError(f"cannot retrieve guest result: {command}\n{reason}")
            output = local.read_bytes().decode(errors="replace").replace("\r", "")
        finally:
            with suppress(subprocess.SubprocessError):
                subprocess.run(ADB + ["shell", f"rm -f {remote} {remote}.part"],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=5)
    match = re.search(r"\n__BRIDGE_RC=(\d+)\s*$", output)
    if not match:
        raise RuntimeError(f"incomplete guest result: {command}\n{output}")
    if int(match[1]):
        raise RuntimeError(f"guest command failed (exit {match[1]}): {command}\n{output[:match.start()]}")
    return output[:match.start()].strip()


def push(source, target):
    subprocess.run(ADB + ["push", str(source), target], check=True, timeout=90)


@contextmanager
def forward_panel(guest_port):
    port = int(subprocess.check_output(ADB + ["forward", "tcp:0", f"tcp:{guest_port}"], timeout=10))
    if not 0 < port < 65536:
        raise ValueError(f"invalid ADB forward port {port}")
    try:
        yield f"http://127.0.0.1:{port}/diagnostics/"
    finally:
        subprocess.run(ADB + ["forward", "--remove", f"tcp:{port}"], check=True, timeout=10)


def browser_case(name, url, out, arch):
    script = Path(__file__).resolve().parents[1] / "web-screenshots" / "kuyase.mjs"
    log = out / f"{arch}-web-{name}.log"
    with log.open("w") as stream:
        result = subprocess.run(["node", str(script), name, url, str(log.with_suffix(".png"))],
                                stdout=stream, stderr=subprocess.STDOUT, timeout=120)
    if result.returncode:
        raise RuntimeError(log.read_text(errors="replace")[-4000:] or f"Browser exited {result.returncode}")


def check_model_picker(arch, out, launch):
    # Run the same package with a synthetic subscription alongside the voice-test process.
    # The trust root belongs only to this Bridge process. The host OS is untouched.
    directory = f"{GUEST}/catalog-{arch}"
    shell(f"mkdir -p {directory} && chmod 700 {directory}")
    with tempfile.TemporaryDirectory() as tmp:
        files = Path(tmp)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2",
                        "-subj", "/CN=api.openai.com", "-addext", "subjectAltName=DNS:api.openai.com",
                        "-keyout", str(files / "catalog.key"), "-out", str(files / "catalog.crt")],
                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, check=True, timeout=30)
        (files / "account.json").write_text(json.dumps({
            "version": 1, "ext_agent_host_id": "kuyase", "issuer": "https://auth.openai.com",
            "subject": "test-user", "client_id": "kuyase", "access_token": "kuyase-subscription",
            "refresh_token": "kuyase-refresh", "scope": "chatgpt.tokens.use.direct offline_access",
            "expires_at": "2100-01-01T00:00:00Z",
        }))
        (files / "config.json").write_text(json.dumps({
            "listen": "127.0.0.1:18446", "diagnostics": {"port": 18447}, "airplay": {"disabled": True},
            "llm_api": "chatgpt", "llm_model": "kuyase-fast", "llm_chatgpt_auth_file": directory + "/account.json",
        }))
        for file in files.iterdir():
            push(file, directory + "/" + file.name)
    shell(f"chmod 600 {directory}/account.json {directory}/catalog.key && "
          f"cat /etc/hosts >{directory}/hosts && echo '127.0.0.1 api.openai.com' >>{directory}/hosts && "
          f"mount -o bind {directory}/hosts /etc/hosts")
    started = []
    try:
        for name, command in [
            ("catalog", f"{GUEST}/control -mode catalog"),
            ("bridge", f"env SSL_CERT_FILE={directory}/catalog.crt GOMAXPROCS=2 {GUEST}/quasar-bridge -config {directory}/config.json"),
        ]:
            proc = launch(ADB + ["shell", f"cd {directory} && echo $$ >{name}.pid && exec {command}"],
                          f"{arch}-web-model-{name}.log")
            started.append((name, proc))
        wait_for_health(18446, [proc for _, proc in started], "model fixture; see web-model logs")
        with forward_panel(18447) as panel:
            browser_case("models", panel, out, arch)
    finally:
        try:
            for name, proc in reversed(started):
                if proc.poll() is None:
                    shell(f"kill $(cat {directory}/{name}.pid)")
                    proc.wait(timeout=15)
        finally:
            shell("umount /etc/hosts")


def health(port=8443):
    result = json.loads(shell(f"wget -q -T 4 -O - http://127.0.0.1:{port}/healthz"))
    if result.get("status") != "ok":
        raise RuntimeError(f"unhealthy Bridge: {result}")
    return result


def wait_for_health(port, processes, description):
    deadline = time.monotonic() + 60
    while True:
        try:
            return health(port)
        except (RuntimeError, ValueError, subprocess.SubprocessError):
            if any(p.poll() is not None for p in processes) or time.monotonic() >= deadline:
                raise RuntimeError(f"startup failed for {description}")
            time.sleep(0.5)


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
    parser.add_argument("--board", choices=FIRMWARE, required=True)
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
        # Verify the target before creating even a temporary result file on it.
        hostname = subprocess.check_output(ADB + ["shell", "hostname"], timeout=10).decode().strip()
        if hostname != "qemu-" + args.board:
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
                "llm_model": "kuyase-fixture", "llm_chatgpt_auth_file": f"{GUEST}/account-{arch}.json",
                "routing": {"default_route": "yandex", "classifier": {"enabled": False}}, "assistant_tools": {"enabled": False},
                "dialogue_context": {"enabled": True},
                "conversation_log": {"enabled": True, "path": f"{GUEST}/conversations-{arch}.jsonl"},
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
            before = wait_for_health(8443, [bridge], "Bridge; see failure-bridge.log")
            if before["airplay"]["state"] != "stopped":
                raise RuntimeError(f"unexpected initial AirPlay state: {before}")
            report.check(arch, "health", health)
            for action in ("weather", "music", "timer", "llm", "llm_urls", "text", "dialogue",
                           "isolation", "reset", "home", "failure", "start", "stop"):
                def check_action():
                    output = shell(f"{GUEST}/control -action {action} -timeout 90s", timeout=100)
                    (out / f"{arch}-{action}.txt").write_text(output + "\n")
                report.check(arch, action, check_action)

            with forward_panel(2704) as panel:
                for name in ("overview", "routing", "settings", "chat", "history", "airplay", "logs", "account", "access"):
                    report.check(arch, "web-" + name, lambda: browser_case(name, panel, out, arch))

            report.check(arch, "web-models", lambda: check_model_picker(arch, out, launch))

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
            (out / f"{arch}-conversations.jsonl").write_text(shell(f"cat {GUEST}/conversations-{arch}.jsonl"))
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
