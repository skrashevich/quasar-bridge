#!/usr/bin/env python3
"""Portable archive and Android-launcher checks; no firmware/device writes."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import struct
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("android_package", ROOT / "scripts/build-android-package.py")
PACKAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PACKAGE)


def elf(interpreter=False):
    data = bytearray(120 if interpreter else 64)
    data[:6] = b"\x7fELF\x02\x01"
    data[18:20] = b"\xb7\x00"
    if interpreter:
        struct.pack_into("<Q", data, 32, 64)
        struct.pack_into("<HH", data, 54, 56, 1)
        struct.pack_into("<I", data, 64, 3)
    return bytes(data)


def android_pcm_elf(interpreter=b"/system/bin/linker64\0"):
    data = bytearray(120 + len(interpreter))
    data[:6] = b"\x7fELF\x02\x01"
    data[18:20] = b"\xb7\x00"
    struct.pack_into("<HH", data, 16, 3, 183)
    struct.pack_into("<Q", data, 32, 64)
    struct.pack_into("<HH", data, 54, 56, 1)
    struct.pack_into("<IIQQQQQQ", data, 64, 3, 4, 120, 0, 0, len(interpreter), len(interpreter), 1)
    data[120:] = interpreter
    return bytes(data)


class AndroidPackageTest(unittest.TestCase):
    def test_archive_is_separate_manual_android_payload(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            supplied = root / "bridge"
            supplied.write_bytes(elf())
            original = supplied.read_bytes()
            pcm = root / "android-pcm"
            pcm.write_bytes(android_pcm_elf())
            pcm_original = pcm.read_bytes()
            output = root / "android.tar.gz"
            PACKAGE.build(output=output, binary=supplied, pcm_binary=pcm)
            self.assertEqual(supplied.read_bytes(), original)
            self.assertEqual(pcm.read_bytes(), pcm_original)
            with tarfile.open(output) as archive:
                names = archive.getnames()
                self.assertEqual(len(names), len(set(names)))
                self.assertTrue(all(not Path(name).is_absolute() and ".." not in Path(name).parts for name in names))
                self.assertEqual(archive.extractfile("bin/quasar-bridge").read(), original)
                self.assertEqual(archive.extractfile("bin/android-pcm").read(), pcm_original)
                self.assertEqual(archive.extractfile("PLATFORM").read(), b"android\n")
                self.assertEqual(archive.extractfile("VERSION").read(), b"dev\n")
                self.assertEqual(archive.extractfile("ARCH").read(), b"arm64\n")
                for name in ["LICENSE", "NOTICE", "THIRD_PARTY_NOTICES", "SOURCE.md", *PACKAGE.PAYLOAD]:
                    self.assertIn(name, names)
                self.assertTrue(any(name.startswith("docs/licenses/") for name in names))
                self.assertNotIn("S96quasar-bridge", names)
                self.assertNotIn("firmware.sh", names)
                self.assertEqual([name for name in names if name.startswith("bin/")], ["bin/quasar-bridge", "bin/android-pcm"])
                sources = [path for path in (ROOT / "packaging/android/pcm").glob("*")
                           if path.is_file() and path.suffix in (".c", ".h", ".md", ".txt")]
                self.assertTrue(sources)
                for source in sources:
                    bundled = archive.extractfile("src/" + str(source.relative_to(ROOT))).read()
                    self.assertEqual(hashlib.sha256(bundled).digest(), hashlib.sha256(source.read_bytes()).digest())
                self.assertEqual(archive.extractfile("src/scripts/build-android-pcm.py").read(),
                                 (ROOT / "scripts/build-android-pcm.py").read_bytes())
                self.assertEqual(archive.extractfile("src/LICENSE").read(), (ROOT / "LICENSE").read_bytes())
                for member in archive.getmembers():
                    self.assertTrue(member.isfile())
                    self.assertEqual(member.mode, 0o755 if member.name.endswith(".sh") or member.name.startswith("bin/") else 0o644)
                config = json.load(archive.extractfile("config.device.example.json"))
                self.assertEqual(config["listen"], "127.0.0.1:8443")
                self.assertEqual(config["mode"], "observe")
                self.assertFalse(config["diagnostics"]["network_access"])
                self.assertTrue(config["airplay"]["disabled"])
            self.assertEqual(output.with_suffix(".gz.sha256").read_text(),
                             f"{hashlib.sha256(output.read_bytes()).hexdigest()}  android.tar.gz\n")

    def test_rejects_wrong_architecture_dynamic_or_truncated_elf(self):
        samples = [b"not elf", elf(interpreter=True)]
        wrong = bytearray(elf())
        wrong[18:20] = b"\x3e\x00"
        samples.append(bytes(wrong))
        truncated = bytearray(elf(interpreter=True))
        struct.pack_into("<Q", truncated, 32, 1000)
        samples.append(bytes(truncated))
        with tempfile.TemporaryDirectory() as tmp:
            binary = Path(tmp) / "bridge"
            for data in samples:
                binary.write_bytes(data)
                with self.subTest(data=data[:20]), self.assertRaises(ValueError):
                    PACKAGE.build(output=Path(tmp) / "bad.tar.gz", binary=binary)
                self.assertFalse((Path(tmp) / "bad.tar.gz").exists())

    def test_shell_scripts_parse(self):
        for script in (ROOT / "packaging/android").glob("*.sh"):
            with self.subTest(script=script.name):
                self.assertTrue(script.read_text().startswith("#!/system/bin/sh\n"))
                subprocess.run(["sh", "-n", str(script)], check=True, capture_output=True)

    def fixture(self, root, dns="192.0.2.53"):
        root = root.resolve()
        package = root / "package"
        package.mkdir()
        for name in PACKAGE.PAYLOAD:
            shutil.copy2(ROOT / "packaging/android" / name, package / name)
        (package / "bin").mkdir()
        bridge = package / "bin/quasar-bridge"
        bridge.write_text('#!/bin/sh\n'
                          'if [ \"$1\" = -v ]; then echo dev; exit 0; fi\n'
                          'printf "%s\\n" "DNS=$QUASAR_DNS_SERVER" "RELAY=$QUASAR_ANDROID_BACKEND_PROXY" "CA=$SSL_CERT_FILE" "APLAY=$QUASAR_AIRPLAY_APLAY" "TMP=$TMPDIR" "PWD=$PWD"\n'
                          'printf "ARG=%s\\n" "$@"\n')
        bridge.chmod(0o755)
        pcm = package / "bin/android-pcm"
        pcm.write_bytes(android_pcm_elf())
        pcm.chmod(0o644)
        for name, value in [("ARCH", "arm64"), ("PLATFORM", "android"), ("VERSION", "dev"),
                            ("LICENSE", "license"), ("NOTICE", "notice"),
                            ("THIRD_PARTY_NOTICES", "notices"), ("SOURCE.md", "source")]:
            (package / name).write_text(value + "\n")
        (package / "docs").mkdir()
        (package / "src").mkdir()
        (package / "src/helper-source.txt").write_text("native helper source\n")
        tools = root / "tools"
        tools.mkdir()
        for name, body in {
            "getprop": f'case "$1" in net.dns1) echo "{dns}" ;; ro.build.version.sdk) echo 28 ;; esac',
            "id": "echo 0",
            "uname": "echo aarch64",
        }.items():
            path = tools / name
            path.write_text("#!/bin/sh\n" + body + "\n")
            path.chmod(0o755)
        state = root / "state"
        state.mkdir()
        (state / "config.json").write_text('{}\n')
        env = {key: value for key, value in os.environ.items() if not key.startswith("QUASAR_") and key != "SSL_CERT_FILE"}
        env.update(PATH=str(tools) + os.pathsep + os.environ["PATH"], QUASAR_DATA_DIR=str(state))
        return package, state, env

    def test_launcher_uses_android_dns_and_private_temp(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env = self.fixture(Path(tmp))
            result = subprocess.run(["sh", str(package / "start.sh"), "-check-config"], env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("DNS=192.0.2.53\n", result.stdout)
            self.assertIn("RELAY=1\n", result.stdout)
            self.assertIn("CA=/system/vendor/quasar/ca-certificates.crt\n", result.stdout)
            self.assertIn(f"APLAY={package}/bin/android-pcm\n", result.stdout)
            self.assertIn(f"TMP={state}/tmp\n", result.stdout)
            self.assertIn(f"PWD={state}\n", result.stdout)
            self.assertIn("ARG=-check-config\n", result.stdout)
            self.assertTrue((state / "tmp").is_dir())

    def test_launcher_honors_explicit_dns_and_ca(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env = self.fixture(Path(tmp))
            env.update(QUASAR_DNS_SERVER="[2001:db8::53]:5353", SSL_CERT_FILE="/custom/ca.crt", QUASAR_ANDROID_BACKEND_PROXY="0", QUASAR_AIRPLAY_APLAY="/custom/android-player")
            result = subprocess.run(["sh", str(package / "start.sh"), "-check-config"], env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("DNS=[2001:db8::53]:5353\n", result.stdout)
            self.assertIn("CA=/custom/ca.crt\n", result.stdout)
            self.assertIn("APLAY=/custom/android-player\n", result.stdout)
            self.assertIn("RELAY=1\n", result.stdout)

    def test_missing_android_dns_fails_before_launch(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env = self.fixture(Path(tmp), dns="")
            result = subprocess.run(["sh", str(package / "start.sh"), "-check-config"], env=env, text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("set QUASAR_DNS_SERVER", result.stderr)
            self.assertEqual(result.stdout, "")

    def test_install_preserves_existing_config_and_does_not_start(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env = self.fixture(Path(tmp))
            original = (state / "config.json").read_bytes()
            result = subprocess.run(["sh", str(package / "install.sh")], env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual((state / "config.json").read_bytes(), original)
            self.assertTrue((state / "app/bin/quasar-bridge").is_file())
            self.assertTrue((state / "app/bin/android-pcm").is_file())
            self.assertEqual((state / "app/bin/android-pcm").stat().st_mode & 0o777, 0o755)
            self.assertEqual((state / "app/src/helper-source.txt").read_text(), "native helper source\n")
            self.assertFalse((state / "bridge.pid").exists())
            self.assertFalse((state / "endpoint-mount").exists())
            self.assertEqual(state.stat().st_mode & 0o777, 0o700)
            self.assertEqual((state / "config.json").stat().st_mode & 0o777, 0o600)
            second = subprocess.run(["sh", str(package / "install.sh")], env=env, text=True, capture_output=True)
            self.assertNotEqual(second.returncode, 0)
            self.assertIn("installation exists", second.stderr)


    def test_install_rejects_stable_binary_before_creating_app(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env = self.fixture(Path(tmp))
            bridge = package / "bin/quasar-bridge"
            bridge.write_text(bridge.read_text().replace("echo dev", "echo 0.8.0"))
            result = subprocess.run(["sh", str(package / "install.sh")], env=env, text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("require a dev binary", result.stderr)
            self.assertFalse((state / "app").exists())

    def overlay_fixture(self, root, unrelated=False):
        root = root.resolve()
        package, state, env = self.fixture(root)
        target = root / "quasar-prod.cfg"
        original = b'{"aliced":{"uniProxyUrl":"wss://original.example/uni.ws"}}\n'
        target.write_bytes(original)
        mounts = root / "mountinfo"
        mounts.write_text("99 1 0:1 /unrelated " + str(target) + " rw - ext4 /device rw\n" if unrelated else "1 0 0:1 / / rw - ext4 /device rw\n")
        # Project only the kernel mount table path into a fixture. All endpoint
        # and ownership decisions still run through the shipped control script.
        control = package / "control.sh"
        control.write_text(control.read_text().replace("/proc/self/mountinfo", str(mounts)))
        tools = root / "tools"
        readlink = shutil.which("readlink")
        (tools / "readlink").write_text(
            '#!/bin/sh\n'
            f'case "$1" in /proc/{os.getpid()}/exe) echo "{package}/bin/quasar-bridge" ;; *) exec "{readlink}" "$@" ;; esac\n')
        (tools / "readlink").chmod(0o755)
        stat_tool = tools / "stat"
        stat_tool.write_text('#!/usr/bin/env python3\n'
                             'import os,sys\n'
                             'assert sys.argv[1:3] == ["-c", "%d:%i"]\n'
                             's=os.stat(sys.argv[3]); print(str(s.st_dev)+":"+str(s.st_ino))\n')
        stat_tool.chmod(0o755)
        bridge = package / "bin/quasar-bridge"
        bridge.write_text(
            '#!/usr/bin/env python3\n'
            'import os,pathlib,sys\n'
            'if sys.argv[1] == "-speaker-android-cache-rollback":\n'
            ' if os.getenv("QUASAR_TEST_CACHE_FAIL") == "1": sys.exit(1)\n'
            ' path=pathlib.Path(sys.argv[sys.argv.index("-speaker-config")+1])\n'
            ' assert path.read_bytes() == ' + repr(original) + '\n'
            ' pathlib.Path(os.environ["QUASAR_DATA_DIR"], "cache-rollback-called").write_text(sys.argv[2])\n'
            ' sys.exit(0)\n'
            'if sys.argv[1] == "-healthcheck":\n'
            ' if "-healthcheck-android-backend" in sys.argv:\n'
            '  if os.getenv("QUASAR_TEST_NO_RELAY") == "1": sys.exit(1)\n'
            '  pathlib.Path(os.environ["QUASAR_DATA_DIR"], "relay-health-required").touch()\n'
            ' sys.exit(0)\n'
            'assert sys.argv[1:3] == ["-speaker-endpoint", "apply"]\n'
            'assert "-speaker-android" in sys.argv\n'
            'path=pathlib.Path(sys.argv[sys.argv.index("-speaker-config")+1])\n'
            'assert path.name == "speaker-config.json"\n'
            'path.write_text("patched endpoint\\n")\n'
            'pathlib.Path(sys.argv[sys.argv.index("-speaker-backup")+1]).write_text("backup")\n')
        (state / "bridge.pid").write_text(str(os.getpid()))
        env["QUASAR_DEVICE_CONFIG"] = str(target)
        for name, body in {
            "mount": 'test "$1:$2" = -o:bind || exit 2\n'
                     'mv "$4" "$4.original"\n'
                     'ln "$3" "$4"\n'
                     f'printf "99 1 0:1 /source %s rw - ext4 /device rw\\n" "$4" > "{mounts}"\n',
            "umount": 'mv "$1.original" "$1"\n'
                      f'printf "1 0 0:1 / / rw - ext4 /device rw\\n" > "{mounts}"\n',
        }.items():
            path = tools / name
            path.write_text("#!/bin/sh\n" + body)
            path.chmod(0o755)
        return package, state, env, target, original, mounts

    def test_overlay_roundtrip_preserves_underlying_firmware(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env, target, original, mounts = self.overlay_fixture(Path(tmp))
            apply = subprocess.run(["sh", str(package / "control.sh"), "apply"], env=env, text=True, capture_output=True)
            self.assertEqual(apply.returncode, 0, apply.stderr)
            self.assertEqual(target.read_text(), "patched endpoint\n")
            self.assertEqual(Path(str(target) + ".original").read_bytes(), original)
            self.assertTrue((state / "endpoint-mount").exists())
            self.assertTrue((state / "relay-health-required").exists())
            rollback = subprocess.run(["sh", str(package / "control.sh"), "rollback"], env=env, text=True, capture_output=True)
            self.assertEqual(rollback.returncode, 0, rollback.stderr)
            self.assertEqual(target.read_bytes(), original)
            self.assertEqual((state / "cache-rollback-called").read_text(), "/data/quasar/data/user_config.dat")
            self.assertEqual(mounts.read_text(), "1 0 0:1 / / rw - ext4 /device rw\n")
            self.assertFalse((state / "endpoint-mount").exists())
            self.assertFalse((state / "endpoint-backup.json").exists())

    def test_overlay_refuses_unrelated_existing_mount(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env, target, original, mounts = self.overlay_fixture(Path(tmp), unrelated=True)
            result = subprocess.run(["sh", str(package / "control.sh"), "apply"], env=env, text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("already has a mount", result.stderr)
            self.assertEqual(target.read_bytes(), original)
            self.assertFalse((state / "endpoint-mount").exists())

    def test_rollback_refuses_replaced_overlay(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env, target, original, mounts = self.overlay_fixture(Path(tmp))
            apply = subprocess.run(["sh", str(package / "control.sh"), "apply"], env=env, text=True, capture_output=True)
            self.assertEqual(apply.returncode, 0, apply.stderr)
            target.unlink()
            target.write_text("unrelated replacement\n")
            result = subprocess.run(["sh", str(package / "control.sh"), "rollback"], env=env, text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("refusing unmount", result.stderr)
            self.assertEqual(target.read_text(), "unrelated replacement\n")
            self.assertTrue((state / "endpoint-mount").exists())

    def test_rollback_clears_record_after_reboot_without_unmount(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env, target, original, mounts = self.overlay_fixture(Path(tmp))
            (state / "endpoint-mount").write_text(str(target) + "\n1:2\n")
            result = subprocess.run(["sh", str(package / "control.sh"), "rollback"], env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("already absent", result.stdout)
            self.assertEqual(target.read_bytes(), original)
            self.assertFalse((state / "endpoint-mount").exists())



    def test_mount_inspection_failures_preserve_recovery_state(self):
        for failure in ("missing", "directory", "unreadable", "empty", "malformed"):
            for action in ("apply", "rollback"):
                with self.subTest(failure=failure, action=action), tempfile.TemporaryDirectory() as tmp:
                    package, state, env, target, original, mounts = self.overlay_fixture(Path(tmp))
                    record = state / "endpoint-mount"
                    backup = state / "endpoint-backup.json"
                    copy = state / "speaker-config.json"
                    if action == "rollback":
                        record.write_text(str(target) + "\n1:2\n")
                        backup.write_text("backup")
                        copy.write_text("copy")
                    if failure == "missing":
                        mounts.unlink()
                    elif failure == "directory":
                        mounts.unlink()
                        mounts.mkdir()
                    elif failure == "unreadable":
                        mounts.chmod(0)
                    elif failure == "empty":
                        mounts.write_text("")
                    else:
                        mounts.write_text("broken mount table\n")
                    # A failed lookup must never reach either mutating command.
                    for command in ("mount", "umount"):
                        (Path(env["PATH"].split(os.pathsep)[0]) / command).write_text(
                            '#!/bin/sh\n'
                            f'echo mutation > "{state}/unexpected-mutation"\n'
                            'exit 99\n')
                    result = subprocess.run(["sh", str(package / "control.sh"), action], env=env, text=True, capture_output=True)
                    if failure == "unreadable":
                        mounts.chmod(0o600)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("Cannot inspect firmware mounts", result.stderr)
                    self.assertFalse((state / "unexpected-mutation").exists())
                    self.assertEqual(target.read_bytes(), original)
                    if action == "rollback":
                        self.assertEqual(record.read_text(), str(target) + "\n1:2\n")
                        self.assertEqual(backup.read_text(), "backup")
                        self.assertEqual(copy.read_text(), "copy")
                    else:
                        self.assertFalse(record.exists())
                        self.assertFalse(backup.exists())
                        self.assertFalse(copy.exists())


    def test_overlay_refuses_bridge_without_android_backend_relay(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env, target, original, mounts = self.overlay_fixture(Path(tmp))
            env["QUASAR_TEST_NO_RELAY"] = "1"
            result = subprocess.run(["sh", str(package / "control.sh"), "apply"], env=env, text=True, capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("with the Android backend relay", result.stderr)
            self.assertEqual(target.read_bytes(), original)
            self.assertFalse((state / "endpoint-mount").exists())
            self.assertFalse((state / "endpoint-backup.json").exists())
            self.assertFalse((state / "speaker-config.json").exists())


    def test_cache_rollback_failure_keeps_recovery_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            package, state, env, target, original, mounts = self.overlay_fixture(Path(tmp))
            apply = subprocess.run(["sh", str(package / "control.sh"), "apply"], env=env, text=True, capture_output=True)
            self.assertEqual(apply.returncode, 0, apply.stderr)
            env["QUASAR_TEST_CACHE_FAIL"] = "1"
            rollback = subprocess.run(["sh", str(package / "control.sh"), "rollback"], env=env, text=True, capture_output=True)
            self.assertNotEqual(rollback.returncode, 0)
            self.assertEqual(target.read_bytes(), original)
            self.assertTrue((state / "endpoint-mount").exists())
            self.assertTrue((state / "endpoint-backup.json").exists())
            self.assertTrue((state / "speaker-config.json").exists())
            # A second attempt can repair the cache after the overlay is gone.
            env.pop("QUASAR_TEST_CACHE_FAIL")
            env["QUASAR_NATIVE_CONFIG_STORAGE"] = "/custom/native-config.dat"
            retry = subprocess.run(["sh", str(package / "control.sh"), "rollback"], env=env, text=True, capture_output=True)
            self.assertEqual(retry.returncode, 0, retry.stderr)
            self.assertEqual((state / "cache-rollback-called").read_text(), "/custom/native-config.dat")
            self.assertFalse((state / "endpoint-mount").exists())


    def test_rejects_non_android_pcm_helpers(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            bridge = root / "bridge"
            bridge.write_bytes(elf())
            pcm = root / "pcm"
            wrong_arch = bytearray(android_pcm_elf())
            wrong_arch[18:20] = b"\x3e\x00"
            bad_range = bytearray(android_pcm_elf())
            struct.pack_into("<Q", bad_range, 64 + 8, len(bad_range) + 100)
            for data in (elf(), android_pcm_elf(b"/lib/ld-linux-aarch64.so.1\0"), bytes(wrong_arch), bytes(bad_range)):
                with self.subTest(data=data[:20]):
                    pcm.write_bytes(data)
                    with self.assertRaises(ValueError):
                        PACKAGE.build(output=root / "bad.tar.gz", binary=bridge, pcm_binary=pcm)
                    self.assertEqual(pcm.read_bytes(), data)
                    self.assertFalse((root / "bad.tar.gz").exists())

    def test_builds_native_pcm_when_not_supplied(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            bridge = root / "bridge"
            bridge.write_bytes(elf())
            def build_pcm(command, **kwargs):
                self.assertEqual(command[1:3], [str(ROOT / "scripts/build-android-pcm.py"), "--output"])
                Path(command[3]).write_bytes(android_pcm_elf())
                return subprocess.CompletedProcess(command, 0)
            with mock.patch.object(PACKAGE.subprocess, "run", side_effect=build_pcm) as build:
                output = PACKAGE.build(output=root / "android.tar.gz", binary=bridge)
                build.assert_called_once()
            with tarfile.open(output) as archive:
                self.assertEqual(archive.extractfile("bin/android-pcm").read(), android_pcm_elf())

if __name__ == "__main__":
    unittest.main()
