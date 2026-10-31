#!/usr/bin/env python3
"""Package and BusyBox installation regression checks; no speaker required."""
import hashlib
import http.server
import os
from pathlib import Path
import json
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading
import unittest
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


class PackageTest(unittest.TestCase):
    def test_upx_packages_compressed_copy_and_checks_integrity(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            binary = root / "binary"
            original = b"\x7fELF\x02" + bytes(13) + b"\xb7\x00" + bytes(44)
            binary.write_bytes(original)
            upx = root / "upx"
            upx.write_text(
                '#!/usr/bin/env python3\n'
                'from pathlib import Path\nimport sys\n'
                'binary = Path(sys.argv[-1])\n'
                'if sys.argv[1:-1] == ["--ultra-brute", "--no-lzma"]:\n'
                '    binary.write_bytes(binary.read_bytes() + b"packed")\n'
                'elif sys.argv[1:-1] == ["-t"]:\n'
                '    assert binary.read_bytes().endswith(b"packed")\n'
                '    binary.write_bytes(binary.read_bytes() + b"tested")\n'
                'else:\n    sys.exit(99)\n'
            )
            upx.chmod(0o755)
            output = root / "packed.tar.gz"
            result = subprocess.run([
                "python3", str(ROOT / "scripts/build-speaker-package.py"),
                "--binary", str(binary), "--output", str(output), "--upx",
            ], env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"]),
                capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(binary.read_bytes(), original)
            with tarfile.open(output) as archive:
                self.assertEqual(archive.extractfile("bin/quasar-bridge").read(),
                                 original + b"packedtested")
            self.assertEqual(output.with_suffix(".gz.sha256").read_text(),
                             f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n")

    def test_upx_failure_does_not_produce_archive(self):
        for failing_stage in ("compression", "integrity"):
            with self.subTest(stage=failing_stage), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                binary = root / "binary"
                binary.write_bytes(b"\x7fELF\x02" + bytes(13) + b"\xb7\x00" + bytes(44))
                upx = root / "upx"
                upx.write_text('#!/bin/sh\n' + (
                    'exit 1\n' if failing_stage == "compression" else
                    'if [ "$1" = "-t" ]; then exit 1; fi\nexit 0\n'))
                upx.chmod(0o755)
                output = root / "bad.tar.gz"
                result = subprocess.run([
                    "python3", str(ROOT / "scripts/build-speaker-package.py"),
                    "--binary", str(binary), "--output", str(output), "--upx",
                ], env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"]),
                    capture_output=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(output.exists())
                self.assertFalse(output.with_suffix(".gz.sha256").exists())

    def test_archives_contain_only_public_payload_and_matching_binary(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for arch, elf in (("arm64", b"\x02\xb7\x00"), ("armv7", b"\x01\x28\x00")):
                binary = root / "binary"
                binary.write_bytes(b"\x7fELF" + elf[:1] + bytes(13) + elf[1:] + bytes(44))
                output = root / f"{arch}.tar.gz"
                result = subprocess.run([
                    "python3", str(ROOT / "scripts/build-speaker-package.py"),
                    "--arch", arch, "--binary", str(binary), "--output", str(output),
                    "--tag", "v1.2.3",
                ], capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                with tarfile.open(output) as archive:
                    self.assertEqual(set(archive.getnames()), {
                        "bin/quasar-bridge", "start.sh", "control.sh", "install.sh",
                        "uninstall.sh", "setup.sh", "S96quasar-bridge", "firmware.sh",
                        "config.device.example.json", "ARCH", "VERSION",
                        "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES",
                    })
                    self.assertEqual(archive.extractfile("ARCH").read(), (arch + "\n").encode())
                    self.assertEqual(archive.extractfile("VERSION").read(), b"1.2.3\n")
                    self.assertEqual(archive.extractfile("bin/quasar-bridge").read(), binary.read_bytes())
                    self.assertEqual(archive.getmember("install.sh").mode, 0o755)
                self.assertEqual(output.with_suffix(".gz.sha256").read_text(),
                                 f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n")

    def test_wrong_architecture_does_not_produce_archive(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            binary = root / "binary"
            binary.write_bytes(b"\x7fELF\x02" + bytes(13) + b"\x3e\x00" + bytes(44))
            output = root / "bad.tar.gz"
            result = subprocess.run([
                "python3", str(ROOT / "scripts/build-speaker-package.py"),
                "--arch", "arm64", "--binary", str(binary), "--output", str(output),
            ], capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(output.exists())


@unittest.skipUnless(sys.platform == "linux" and os.geteuid() == 0,
                     "installation tests require Linux root (run in a disposable container)")
class InstallationTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.state = self.root / "state"
        self.init = self.root / "init.d"
        self.init.mkdir()
        self.package = self.root / "package"
        shutil.copytree(ROOT / "packaging/speaker", self.package)
        (self.package / "bin").mkdir()
        shutil.copy2(os.environ["QUASAR_TEST_BINARY"], self.package / "bin/quasar-bridge")
        (self.package / "bin/quasar-bridge").chmod(0o755)
        machine = os.uname().machine
        (self.package / "ARCH").write_text("armv7\n" if machine in ("armv7l", "armv8l") else "arm64\n")
        (self.package / "VERSION").write_text("fixture\n")
        for name in ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES"):
            shutil.copy2(ROOT / name, self.package / name)
        self.env = dict(os.environ, QUASAR_DATA_DIR=str(self.state), QUASAR_INIT_DIR=str(self.init))
        if machine not in ("aarch64", "arm64", "armv7l", "armv8l"):
            # The lifecycle fixture uses a native binary; simulate only the speaker's uname.
            shim = self.root / "shim"
            shim.mkdir()
            (shim / "uname").write_text('#!/bin/sh\necho aarch64\n')
            (shim / "uname").chmod(0o755)
            self.env["PATH"] = str(shim) + os.pathsep + self.env["PATH"]
        self.addCleanup(self.stop)

    def run_script(self, path, *args, ok=True, input_text=None):
        result = subprocess.run(["sh", str(path), *args], env=self.env,
                                capture_output=True, text=True, timeout=45, input=input_text)
        if ok:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        return result

    def control(self, action, **kwargs):
        return self.run_script(self.state / "app/control.sh", action, **kwargs)

    def install(self, **kwargs):
        return self.run_script(self.package / "install.sh", **kwargs)

    def stop(self):
        if (self.state / "app/control.sh").exists():
            self.control("stop")

    def test_install_update_and_uninstall_keep_state_and_boot_independently(self):
        self.install()
        self.control("status")
        self.control("restart")
        config = self.state / "config.json"
        data = json.loads(config.read_text())
        data["dialogue_context"]["max_turns"] = 9
        config.write_text(json.dumps(data))
        runtime = self.state / "runtime.env"
        runtime.write_text("GOMAXPROCS=2\n")
        backup = self.state / "endpoint-backup.json"
        backup.write_text("saved endpoint fixture")
        self.install()
        self.assertEqual(json.loads(config.read_text())["dialogue_context"]["max_turns"], 9)
        self.assertEqual(runtime.read_text(), "GOMAXPROCS=2\n")
        self.assertEqual(backup.read_text(), "saved endpoint fixture")
        self.assertEqual(runtime.stat().st_mode & 0o777, 0o600)
        self.control("stop")
        # Persistent state can contain a service lock left by an older boot.
        (self.state / ".service-lock").mkdir()
        self.run_script(self.init / "S96quasar-bridge", "start")
        self.control("status")
        self.run_script(self.state / "app/uninstall.sh", ok=False)
        self.control("status")
        backup.unlink()  # Simulate a completed endpoint rollback/reboot.
        self.run_script(self.state / "app/uninstall.sh")
        self.assertFalse((self.state / "app").exists())
        self.assertFalse((self.init / "S96quasar-bridge").exists())
        self.assertTrue(config.exists())
        self.assertTrue(runtime.exists())

    def test_bad_config_rejected_before_working_process_is_stopped(self):
        self.install()
        old_pid = (self.state / "bridge.pid").read_text()
        config = self.state / "config.json"
        original = config.read_text()
        config.write_text('{"unknown_option":true}')
        self.install(ok=False)
        self.assertEqual((self.state / "bridge.pid").read_text(), old_pid)
        self.assertFalse((self.state / ".install-lock").exists())
        config.write_text(original)
        self.control("status")

    def test_init_starts_bridge_before_midi_quasar(self):
        self.install()
        self.control("stop")
        (self.init / "S97quasar").write_text(
            '#!/bin/sh\nwget -q -T 2 -O /dev/null http://127.0.0.1:8443/healthz\n')
        result = subprocess.run([
            "sh", "-ec", 'for hook in "$1"/S??*; do sh "$hook" start; done', "sh", str(self.init),
        ], env=self.env, capture_output=True, text=True, timeout=30)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.control("status")

    def test_failed_update_restores_previous_binary_and_service(self):
        self.install()
        original = (self.state / "app/bin/quasar-bridge").read_bytes()
        # A new launcher that validates but cannot run reproduces a broken update.
        (self.package / "start.sh").write_text(
            '#!/bin/sh\ncase "${1:-}" in -check-config) exit 0 ;; *) exit 1 ;; esac\n')
        self.install(ok=False)
        self.control("status")
        self.assertEqual((self.state / "app/bin/quasar-bridge").read_bytes(), original)
        self.assertFalse((self.state / ".install-lock").exists())

    def test_hook_write_failure_restores_read_only_mount_and_previous_service(self):
        self.install()
        original_hook = (self.init / "S96quasar-bridge").read_text()
        shim = self.root / "shim"
        shim.mkdir(exist_ok=True)
        (shim / "awk").write_text('#!/bin/sh\nexit 0\n')  # Simulate a read-only root.
        (shim / "mount").write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$QUASAR_MOUNT_TRACE"\n')
        (shim / "awk").chmod(0o755)
        (shim / "mount").chmod(0o755)
        self.env["PATH"] = str(shim) + os.pathsep + self.env["PATH"]
        trace = self.root / "mount-trace"
        self.env["QUASAR_MOUNT_TRACE"] = str(trace)
        (self.init / "S96quasar-bridge.new").mkdir()  # Force publication to fail after start.
        self.install(ok=False)
        self.control("status")
        self.assertEqual((self.init / "S96quasar-bridge").read_text(), original_hook)
        self.assertEqual(trace.read_text().splitlines(), ["-o remount,rw /", "-o remount,ro /"])
        self.assertFalse((self.state / ".install-lock").exists())

    def test_stale_pid_does_not_signal_unrelated_process(self):
        self.install()
        self.control("stop")
        process = subprocess.Popen(["sleep", "60"])
        try:
            (self.state / "bridge.pid").write_text(str(process.pid))
            self.control("stop")
            self.assertIsNone(process.poll())
        finally:
            process.terminate()
            process.wait()

    def test_apply_and_rollback_preserve_other_firmware_fields(self):
        self.install()
        firmware = self.root / "firmware.json"
        original = {"unrelated": {"keep": True}, "aliced": {
            "uniProxyUrl": "wss://uniproxy.alice.yandex.net/uni.ws",
            "uniProxyQuicUrl": "quic://example.invalid", "keep": 42,
        }}
        firmware.write_text(json.dumps(original))
        self.env["QUASAR_DEVICE_CONFIG"] = str(firmware)
        self.control("apply")
        changed = json.loads(firmware.read_text())
        self.assertEqual(changed["aliced"]["uniProxyUrl"], "ws://127.0.0.1:8443/uni.ws")
        self.assertEqual(changed["aliced"]["uniProxyQuicUrl"], "")
        self.control("apply")  # Must not overwrite the original backup.
        self.control("stop")
        self.control("rollback")  # Recovery also works when the bridge is down.
        self.assertEqual(json.loads(firmware.read_text()), original)
        self.assertFalse((self.state / "endpoint-backup.json").exists())

    def serve(self, handler):
        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(server.server_close)
        self.addCleanup(server.shutdown)
        return f"http://127.0.0.1:{server.server_port}"

    def serve_directory(self, directory):
        class Handler(http.server.SimpleHTTPRequestHandler):
            def __init__(self, *args, **kwargs):
                super().__init__(*args, directory=str(directory), **kwargs)

            def log_message(self, *args):
                pass

        return self.serve(Handler)

    def test_download_verifies_checksum_before_running_setup(self):
        release = self.root / "release"
        release.mkdir()
        setup = self.root / "setup.sh"
        setup.write_text('#!/bin/sh\ntouch "$QUASAR_DATA_DIR/download-complete"\n')
        self.state.mkdir()
        arch = (self.package / "ARCH").read_text().strip()
        archive = release / f"quasar-bridge-linux-{arch}.tar.gz"
        with tarfile.open(archive, "w:gz") as tar:
            tar.add(setup, arcname="setup.sh")
        checksum = archive.with_suffix(".gz.sha256")
        checksum.write_text(f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n")

        self.env["QUASAR_RELEASE_URL"] = self.serve_directory(release)
        self.run_script(ROOT / "install.sh")
        marker = self.state / "download-complete"
        self.assertTrue(marker.exists())
        marker.unlink()
        checksum.write_text(f"{'0' * 64}  {archive.name}\n")
        self.run_script(ROOT / "install.sh", ok=False)
        self.assertFalse(marker.exists())

    def backend_url(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"choices":[{"message":{"content":"ok"}}]}')

            def log_message(self, *args):
                pass

        return self.serve(Handler) + "/v1"

    def test_setup_configures_checks_backend_and_applies_endpoint_without_reboot(self):
        url = self.backend_url()
        firmware = self.root / "firmware.json"
        firmware.write_text('{"aliced":{"uniProxyUrl":"wss://example.invalid/uni.ws"}}')
        self.env["QUASAR_DEVICE_CONFIG"] = str(firmware)
        release = self.root / "release"
        release.mkdir()
        arch = (self.package / "ARCH").read_text().strip()
        archive = release / f"quasar-bridge-linux-{arch}.tar.gz"
        with tarfile.open(archive, "w:gz") as tar:
            for member in self.package.iterdir():
                tar.add(member, arcname=member.name)
        archive.with_suffix(".gz.sha256").write_text(
            f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n")
        self.env["QUASAR_RELEASE_URL"] = self.serve_directory(release)
        key = "fixture'\"\\$`token"
        result = self.run_script(ROOT / "install.sh",
                                 input_text=f"{url}\nfixture-model\n{key}\n1\n\ny\nn\n")
        self.assertNotIn(key, result.stdout + result.stderr)
        self.control("status")
        self.assertEqual(json.loads(firmware.read_text())["aliced"]["uniProxyUrl"],
                         "ws://127.0.0.1:8443/uni.ws")
        env_file = self.state / "runtime.env"
        restored_key = subprocess.check_output([
            "sh", "-c", '. "$1"; printf "%s" "$QUASAR_LLM_API_KEY"', "sh", str(env_file),
        ], text=True)
        self.assertEqual(restored_key, key)
        original = env_file.read_bytes()
        # Exercise host-side download/transfer/update, replacing only SSH transport.
        # Actual SSH authentication and TTY interaction are checked on the MIDI.
        shim = self.root / "shim"
        shim.mkdir(exist_ok=True)
        ssh = shim / "ssh"
        ssh.write_text('''#!/bin/sh
while [ "$#" -gt 0 ]; do
 case "$1" in
  -o) shift 2 ;;
  -O) exit 0 ;;
  -n|-T|-tt) shift ;;
  *) shift; break ;;
 esac
done
exec sh -c "$*"
''')
        ssh.chmod(0o755)
        self.env["PATH"] = str(shim) + os.pathsep + self.env["PATH"]
        self.run_script(ROOT / "install.sh", "root@fixture", input_text="n\n")
        self.assertEqual(env_file.read_bytes(), original)
        self.control("status")

    def test_setup_migrates_only_the_legacy_yam_package_and_process(self):
        physical_state = self.root / "physical-state"
        physical_state.mkdir()
        self.state.symlink_to(physical_state, target_is_directory=True)
        config = {"listen": "127.0.0.1:8443", "mode": "observe",
                  "llm_base_url": self.backend_url(), "llm_model": "fixture-model"}
        (self.state / "config.json").write_text(json.dumps(config))
        legacy = self.root / "yam/plugins/quasar-bridge"
        shutil.copytree(self.package, legacy)
        other = self.root / "yam/plugins/other-app"
        other.mkdir()
        (other / "keep").write_text("unchanged")
        self.env["QUASAR_YAM_PACKAGE_DIR"] = str(legacy)
        process = subprocess.Popen(["sh", str(legacy / "start.sh")], env=self.env,
                                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        try:
            for _ in range(50):
                try:
                    with urllib.request.urlopen("http://127.0.0.1:8443/healthz", timeout=1):
                        break
                except OSError:
                    time.sleep(0.1)
            else:
                self.fail("legacy fixture did not start")
            self.run_script(self.package / "setup.sh", input_text="n\ny\nn\n")
            process.wait(timeout=5)
            self.control("status")
            self.assertFalse(legacy.exists())
            self.assertTrue((self.state / "yam-package.backup/bin/quasar-bridge").exists())
            self.assertEqual((other / "keep").read_text(), "unchanged")
        finally:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=5)

    def test_failed_backend_setup_restores_existing_settings(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers["Content-Length"]))
                self.send_response(401)
                self.end_headers()

            def log_message(self, *args):
                pass

        self.install()
        config = self.state / "config.json"
        original = config.read_bytes()
        runtime = self.state / "runtime.env"
        runtime.write_text("KEEP_FIXTURE=yes\n")
        url = self.serve(Handler) + "/v1"
        self.run_script(self.package / "setup.sh",
                        input_text=f"y\n{url}\nfixture-model\nfixture-key\n1\n\nn\n", ok=False)
        self.assertEqual(config.read_bytes(), original)
        self.assertEqual(runtime.read_text(), "KEEP_FIXTURE=yes\n")
        self.control("status")

    def test_cancelled_setup_keeps_existing_configuration_and_running_service(self):
        self.install()
        config = self.state / "config.json"
        original = config.read_bytes()
        old_pid = (self.state / "bridge.pid").read_text()
        self.run_script(self.package / "setup.sh", input_text="y\n", ok=False)
        self.assertEqual(config.read_bytes(), original)
        self.assertEqual((self.state / "bridge.pid").read_text(), old_pid)
        self.control("status")


if __name__ == "__main__":
    unittest.main()
