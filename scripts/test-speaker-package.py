#!/usr/bin/env python3
"""Package and BusyBox installation regression checks; no speaker required."""
import hashlib
import io
import http.server
import os
import pty
from pathlib import Path
import json
import select
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import termios
import threading
import unittest
import time
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]


class PackageTest(unittest.TestCase):
    def helpers(self, root):
        result = []
        for arch, elf in (("arm64", b"\x02\xb7\x00"), ("armv7", b"\x01\x28\x00")):
            path = root / ("pcm-" + arch)
            path.write_bytes(b"\x7fELF" + elf[:1] + b"\x01" + bytes(12) + elf[1:] + bytes(44))
            result += ["--pcm-" + arch + "-binary", str(path)]
        return result

    def test_upx_packages_compressed_copy_and_checks_integrity(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            binary = root / "binary"
            original = b"\x7fELF\x02\x01" + bytes(12) + b"\xb7\x00" + bytes(44)
            binary.write_bytes(original)
            helpers = self.helpers(root)
            uncompressed = root / "uncompressed.tar.gz"
            subprocess.run([
                "python3", str(ROOT / "scripts/build-speaker-package.py"),
                "--binary", str(binary), "--output", str(uncompressed), *helpers,
            ], check=True, capture_output=True)
            # Repack an existing PCM bundle: UPX must see only the host ELF.
            with tarfile.open(uncompressed) as archive:
                supplied = archive.extractfile("bin/quasar-bridge").read()
            binary.write_bytes(supplied)
            upx = root / "upx"
            upx.write_text(
                '#!/usr/bin/env python3\n'
                'from pathlib import Path\nimport sys\n'
                'binary = Path(sys.argv[-1])\n'
                'if sys.argv[1:-1] == ["-6", "--no-lzma"]:\n'
                '    assert binary.read_bytes() == ' + repr(original) + '\n'
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
                "--binary", str(binary), "--output", str(output), "--upx", *helpers,
            ], env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"]),
                capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(binary.read_bytes(), supplied)
            with tarfile.open(output) as archive:
                bundled = archive.extractfile("bin/quasar-bridge").read()
                self.assertTrue(bundled.startswith(original + b"packedtested"))
                with zipfile.ZipFile(io.BytesIO(bundled)) as pcm:
                    self.assertIsNone(pcm.testzip())
                self.assertEqual([n for n in archive.getnames() if n.startswith("bin/")], ["bin/quasar-bridge"])
            self.assertEqual(output.with_suffix(".gz.sha256").read_text(),
                             f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n")

    def test_upx_failure_does_not_produce_archive(self):
        for failing_stage in ("compression", "integrity"):
            with self.subTest(stage=failing_stage), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                binary = root / "binary"
                binary.write_bytes(b"\x7fELF\x02\x01" + bytes(12) + b"\xb7\x00" + bytes(44))
                upx = root / "upx"
                upx.write_text('#!/bin/sh\n' + (
                    'exit 1\n' if failing_stage == "compression" else
                    'if [ "$1" = "-t" ]; then exit 1; fi\nexit 0\n'))
                upx.chmod(0o755)
                output = root / "bad.tar.gz"
                result = subprocess.run([
                    "python3", str(ROOT / "scripts/build-speaker-package.py"),
                    "--binary", str(binary), "--output", str(output), "--upx", *self.helpers(root),
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
                binary.write_bytes(b"\x7fELF" + elf[:1] + b"\x01" + bytes(12) + elf[1:] + bytes(44))
                output = root / f"{arch}.tar.gz"
                result = subprocess.run([
                    "python3", str(ROOT / "scripts/build-speaker-package.py"),
                    "--arch", arch, "--binary", str(binary), "--output", str(output),
                    "--tag", "v1.2.3", *self.helpers(root),
                ], capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                with tarfile.open(output) as archive:
                    expected = {
                        "bin/quasar-bridge", "start.sh", "control.sh", "install.sh",
                        "uninstall.sh", "setup.sh", "S96quasar-bridge", "firmware.sh",
                        "config.device.example.json", "ARCH", "VERSION",
                        "LICENSE", "NOTICE", "THIRD_PARTY_NOTICES", "SOURCE.md",
                        "docs/licenses/go-aac-LGPL-3.0.txt", "docs/licenses/go-aac-GPL-3.0.txt",
                    }
                    # Dependency license notices may grow independently.
                    expected.update(str(p.relative_to(ROOT)) for p in (ROOT / "docs/licenses").glob("*.txt"))
                    self.assertEqual(set(archive.getnames()), expected)
                    self.assertEqual(archive.extractfile("ARCH").read(), (arch + "\n").encode())
                    self.assertEqual(archive.extractfile("VERSION").read(), b"1.2.3\n")
                    bundled = archive.extractfile("bin/quasar-bridge").read()
                    self.assertTrue(bundled.startswith(binary.read_bytes()))
                    self.assertEqual([n for n in archive.getnames() if n.startswith("bin/")], ["bin/quasar-bridge"])
                    with zipfile.ZipFile(io.BytesIO(bundled)) as pcm:
                        helpers = [pcm.read(n) for n in pcm.namelist() if pcm.read(n).startswith(b"\x7fELF")]
                    expected_helpers = [(root / "pcm-armv7").read_bytes()]
                    if arch == "arm64":
                        expected_helpers.append((root / "pcm-arm64").read_bytes())
                    self.assertCountEqual(helpers, expected_helpers)
                    self.assertEqual(archive.getmember("install.sh").mode, 0o755)
                self.assertEqual(output.with_suffix(".gz.sha256").read_text(),
                                 f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n")

    def test_wrong_architecture_does_not_produce_archive(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            binary = root / "binary"
            binary.write_bytes(b"\x7fELF\x02\x01" + bytes(12) + b"\x3e\x00" + bytes(44))
            output = root / "bad.tar.gz"
            result = subprocess.run([
                "python3", str(ROOT / "scripts/build-speaker-package.py"),
                "--arch", "arm64", "--binary", str(binary), "--output", str(output),
            ], capture_output=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(output.exists())

    def test_wrong_helper_architecture_does_not_produce_archive(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            binary = root / "binary"
            binary.write_bytes(b"\x7fELF\x02\x01" + bytes(12) + b"\xb7\x00" + bytes(44))
            helpers = self.helpers(root)
            (root / "pcm-armv7").write_bytes((root / "pcm-arm64").read_bytes())
            output = root / "bad.tar.gz"
            result = subprocess.run([
                "python3", str(ROOT / "scripts/build-speaker-package.py"),
                "--binary", str(binary), "--output", str(output), *helpers,
            ], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(output.exists())


class ChatGPTSetupTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.package = self.root / "package"
        self.package.mkdir()
        self.state = self.root / "state"
        self.trace = self.root / "commands"
        for name in ("setup.sh", "start.sh", "control.sh"):
            shutil.copy2(ROOT / "packaging/speaker" / name, self.package / name)
        shim = self.root / "shim"
        shim.mkdir()
        (shim / "id").write_text('#!/bin/sh\nprintf "0\\n"\n')
        (shim / "id").chmod(0o755)
        (self.package / "bin").mkdir()
        binary = self.package / "bin/quasar-bridge"
        binary.write_text('''#!/bin/sh
if [ "$1" = -config ]; then shift 2; fi
printf '%s\\n' "$*" >> "$QUASAR_TEST_TRACE"
if [ "$1" = -chatgpt-auth ] && [ "$2" = login ]; then
 test "${QUASAR_TEST_FAIL_LOGIN:-}" != yes || exit 1
 if [ -n "${QUASAR_LLM_CHATGPT_AUTH_FILE:-}" ]; then printf '{}' > "$QUASAR_LLM_CHATGPT_AUTH_FILE"; fi
 printf 'Continue with ChatGPT: open this URL in your browser.\nhttps://auth.example.invalid/authorize?state=fixture\nWaiting for browser sign-in (10 minutes)…\n'
 if [ -n "${QUASAR_TEST_AUTH_WAIT_FILE:-}" ]; then
  while [ ! -e "$QUASAR_TEST_AUTH_WAIT_FILE" ]; do sleep 0.1; done
 fi
 printf 'ChatGPT sign-in completed.\n{"signed_in":true,"email":"fixture@example.invalid"}\n'
fi
if [ -n "${QUASAR_TEST_HOME_TRACE:-}" ]; then
 printf '%s\n' "${QUASAR_HOME_MCP_URL:-}" >> "$QUASAR_TEST_HOME_TRACE"
fi
if [ -n "${QUASAR_TEST_PARAMETERS_TRACE:-}" ]; then
 printf '%s\\n' "${QUASAR_LLM_CHAT_PARAMETERS:-}" >> "$QUASAR_TEST_PARAMETERS_TRACE"
fi
if [ "$1" = -probe-llm-compat ]; then
 test -n "${QUASAR_LLM_MODEL:-}" || exit 2
 if [ -n "${QUASAR_TEST_COMPAT_DIAGNOSTIC:-}" ]; then
  printf '%s\\n' "$QUASAR_TEST_COMPAT_DIAGNOSTIC" >&2
 fi
 if [ "${QUASAR_TEST_FAIL_COMPAT:-}" = yes ]; then
  printf 'LLM compatibility failed: HTTP 400\\n' >&2
  exit 1
 fi
 if [ "${QUASAR_TEST_EMPTY_COMPAT:-}" != yes ]; then
  if [ -n "${QUASAR_TEST_COMPAT_PARAMETERS:-}" ]; then
   printf '%s\\n' "$QUASAR_TEST_COMPAT_PARAMETERS"
  else
   printf '{}\\n'
  fi
 fi
fi
if [ "$1" = -check-backends ] && [ "${QUASAR_TEST_FAIL_BACKEND_ONCE:-}" = yes ] &&
   [ ! -e "$QUASAR_DATA_DIR/backend-attempted" ]; then
 touch "$QUASAR_DATA_DIR/backend-attempted"
 printf 'backend temporarily unavailable\n' >&2
 exit 1
fi
if [ "$1" = -list-models ]; then
 if [ "${QUASAR_TEST_FAIL_MODELS:-}" = yes ]; then
  printf 'unusable\\t"Partial catalog"\\n'
  exit 1
 fi
 if [ "${QUASAR_TEST_EMPTY_MODELS:-}" != yes ]; then
  printf 'fixture-small\\t"Small fixture"\\nfixture-model\\t"Fixture model"\\n'
 fi
fi
''')
        binary.chmod(0o755)
        (self.package / "install.sh").write_text('''#!/bin/sh
printf 'install\\n' >> "$QUASAR_TEST_TRACE"
''')
        self.env = dict(os.environ, QUASAR_DATA_DIR=str(self.state),
                        QUASAR_YAM_PACKAGE_DIR=str(self.root / "absent-yam"),
                        QUASAR_TEST_TRACE=str(self.trace),
                        PATH=str(shim) + os.pathsep + os.environ["PATH"])

    def test_subscription_setup_signs_in_before_model_selection(self):
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=self.env, input="chatgpt\n2\n\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.trace.read_text().splitlines(),
                         ["-chatgpt-auth login", "-list-models", "-check-config", "-check-backends", "install"])
        output = subprocess.check_output([
            "sh", "-c", '. "$1"; printf "%s\\n" "$QUASAR_LLM_API" '
            '"$QUASAR_LLM_BASE_URL" "$QUASAR_LLM_MODEL" '
            '"$QUASAR_LLM_API_KEY" "$QUASAR_LLM_CHATGPT_AUTH_FILE"',
            "sh", str(self.state / "runtime.env"),
        ], text=True)
        self.assertEqual(output.splitlines(), ["chatgpt", "https://api.openai.com/v1",
                         "fixture-model", "", str(self.state / "chatgpt-auth.json")])
        self.assertTrue((self.state / "chatgpt-auth.json").exists())
        self.assertIn("ssh -N -L 1455:127.0.0.1:1455", result.stdout)
        self.assertIn("1 — fixture-small", result.stdout)
        self.assertIn("2 — fixture-model", result.stdout)

    def test_failed_sign_in_does_not_check_backend_or_install(self):
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_FAIL_LOGIN="yes"),
                                input="chatgpt\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.trace.read_text().splitlines(),
                         ["-chatgpt-auth login"])
        self.assertFalse((self.state / "runtime.env").exists())
        self.assertFalse((self.state / "config.json").exists())

    def test_standard_api_selects_catalog_model_and_keeps_secret(self):
        key = "fixture'\"\\$`token"
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=self.env,
                                input=f"https://llm.example.invalid/v1\n{key}\n2\n2\n\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.trace.read_text().splitlines(),
                         ["-list-models", "-check-config", "-check-backends", "install"])
        output = subprocess.check_output([
            "sh", "-c", '. "$1"; printf "%s\\n" "$QUASAR_LLM_API" "$QUASAR_LLM_API_KEY" "$QUASAR_LLM_MODEL"',
            "sh", str(self.state / "runtime.env"),
        ], text=True)
        self.assertEqual(output.splitlines(), ["responses", key, "fixture-model"])
        self.assertNotIn(key, result.stdout + result.stderr)

    def test_chat_api_probes_and_saves_shell_escaped_parameters(self):
        parameters = json.dumps({"chat_template_kwargs": None,
                                 "temperature": "fixture'\"\\$`value"}, separators=(",", ":"))
        key = "private-api-key"
        values = self.root / "parameters"
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_COMPAT_PARAMETERS=parameters,
                                         QUASAR_TEST_PARAMETERS_TRACE=str(values)),
                                input=f"https://llm.example.invalid/v1\n{key}\n1\n2\n\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.trace.read_text().splitlines(),
                         ["-list-models", "-probe-llm-compat", "-check-config", "-check-backends", "install"])
        output = subprocess.check_output([
            "sh", "-c", '. "$1"; printf "%s" "$QUASAR_LLM_CHAT_PARAMETERS"',
            "sh", str(self.state / "runtime.env"),
        ], text=True)
        self.assertEqual(output, parameters)
        self.assertEqual(values.read_text().splitlines(), ["{}", "{}", parameters, parameters])
        self.assertNotIn(key, result.stdout + result.stderr)
        self.assertEqual((self.state / "runtime.env").stat().st_mode & 0o777, 0o600)

    @unittest.skipUnless(os.environ.get("QUASAR_TEST_BINARY"),
                         "real setup integration requires a native QUASAR_TEST_BINARY")
    def test_real_binary_setup_recovers_google_array_error_and_preserves_settings(self):
        shutil.copy2(os.environ["QUASAR_TEST_BINARY"], self.package / "bin/quasar-bridge")
        (self.package / "bin/quasar-bridge").chmod(0o755)
        provider_error = json.dumps([{"error": {
            "code": 400,
            "message": 'Invalid JSON payload received. Unknown name "chat_template_kwargs": Cannot find field.',
            "status": "INVALID_ARGUMENT",
            "details": [{"@type": "type.googleapis.com/google.rpc.BadRequest",
                         "fieldViolations": [{"description": 'Unknown name "chat_template_kwargs".'}]}],
        }}])
        for existing in (False, True):
            with self.subTest(existing=existing):
                shutil.rmtree(self.state, ignore_errors=True)
                self.trace.unlink(missing_ok=True)
                self.state.mkdir()
                original_config = ('{"mode":"observe","dialogue_context":{"max_turns":9},'
                                   '"local_progress":{"enabled":false}}')
                if existing:
                    (self.state / "config.json").write_text(original_config)
                (self.state / "runtime.env").write_text("KEEP_FIXTURE=yes\n")
                requests = []

                class Handler(http.server.BaseHTTPRequestHandler):
                    def do_GET(self):
                        requests.append((self.path, self.headers.get("Authorization"), None))
                        self.send_response(200)
                        self.send_header("Content-Type", "application/json")
                        self.end_headers()
                        self.wfile.write(b'{"data":[{"id":"models/gemini-fixture","name":"Gemini fixture"}]}')

                    def do_POST(self):
                        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                        requests.append((self.path, self.headers.get("Authorization"), body))
                        if "chat_template_kwargs" in body:
                            self.send_response(400)
                            self.send_header("Content-Type", "application/json")
                            self.end_headers()
                            self.wfile.write(provider_error.encode())
                            return
                        self.send_response(200)
                        self.send_header("Content-Type", "application/json")
                        self.end_headers()
                        content = '{"ok":true}' if "response_format" in body else "ok"
                        self.wfile.write(json.dumps({"choices": [{"message": {"content": content}}]}).encode())

                    def log_message(self, *args):
                        pass

                server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
                thread = threading.Thread(target=server.serve_forever, daemon=True)
                thread.start()
                url = f"http://127.0.0.1:{server.server_port}/v1beta/openai"
                answers = ("y\n" if existing else "") + f"{url}\nfixture-key\n1\n1\n"
                answers += "n\n" if existing else "\nn\n"
                try:
                    result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                            env=self.env, input=answers,
                                            capture_output=True, text=True, timeout=30)
                finally:
                    server.shutdown()
                    server.server_close()
                    thread.join(timeout=5)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertIn(provider_error, result.stdout + result.stderr)
                self.assertEqual(self.trace.read_text().splitlines(), ["install"])
                output = subprocess.check_output([
                    "sh", "-c", '. "$1"; printf "%s\\n" "$QUASAR_LLM_CHAT_PARAMETERS" "$KEEP_FIXTURE"',
                    "sh", str(self.state / "runtime.env"),
                ], text=True).splitlines()
                self.assertEqual(json.loads(output[0]), {"chat_template_kwargs": None})
                self.assertEqual(output[1], "yes")
                if existing:
                    self.assertEqual((self.state / "config.json").read_text(), original_config)
                else:
                    self.assertEqual(json.loads((self.state / "config.json").read_text())["listen"],
                                     "127.0.0.1:8443")
                self.assertTrue(all(auth == "Bearer fixture-key" for _, auth, _ in requests))
                self.assertEqual(requests[0][0], "/v1beta/openai/models")
                self.assertTrue(all(path in ("/v1beta/openai/models", "/v1beta/openai/chat/completions")
                                    for path, _, _ in requests))
                posts = [body for path, _, body in requests if path.endswith("/chat/completions")]
                self.assertEqual(sum("chat_template_kwargs" in body for body in posts), 1)
                self.assertTrue(any("response_format" in body for body in posts))
                self.assertTrue(all(body["model"] == "models/gemini-fixture" for body in posts))
                self.assertNotIn("chat_template_kwargs", posts[-1])

    def test_successful_probe_keeps_provider_errors_visible(self):
        diagnostic = 'HTTP 400: {"error":{"message":"Unknown name chat_template_kwargs","status":"INVALID_ARGUMENT"}}'
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_COMPAT_DIAGNOSTIC=diagnostic,
                                         QUASAR_TEST_COMPAT_PARAMETERS='{"chat_template_kwargs":null}'),
                                input="https://llm.example.invalid/v1\n\n1\n2\n\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(diagnostic, result.stderr)
        self.assertNotIn(diagnostic, (self.state / "runtime.env").read_text())

    def test_failed_probe_restores_existing_settings_without_installing(self):
        self.state.mkdir()
        config = '{"mode":"observe","llm_model":"old-model"}'
        runtime = 'KEEP_FIXTURE=yes\nQUASAR_LLM_CHAT_PARAMETERS=old-value\n'
        (self.state / "config.json").write_text(config)
        (self.state / "runtime.env").write_text(runtime)
        diagnostic = 'HTTP 400: {"error":{"message":"provider failure details","status":"INVALID_ARGUMENT"}}'
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_FAIL_COMPAT="yes",
                                         QUASAR_TEST_COMPAT_DIAGNOSTIC=diagnostic),
                                input="y\nhttps://llm.example.invalid/v1\n\n1\n2\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.trace.read_text().splitlines(), ["-list-models", "-probe-llm-compat"])
        self.assertIn(diagnostic, result.stderr)
        self.assertEqual((self.state / "config.json").read_text(), config)
        self.assertEqual((self.state / "runtime.env").read_text(), runtime)

    def test_empty_probe_result_does_not_install(self):
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_EMPTY_COMPAT="yes"),
                                input="https://llm.example.invalid/v1\n\n1\n2\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn("install", self.trace.read_text().splitlines())
        self.assertFalse((self.state / "runtime.env").exists())

    def test_probe_failure_can_switch_to_responses_and_clear_stale_parameters(self):
        self.state.mkdir()
        (self.state / "config.json").write_text('{"mode":"observe"}')
        (self.state / "endpoint-backup.json").write_text("existing Alice connection")
        (self.state / "runtime.env").write_text(
            '# BEGIN QUASAR SETUP\nQUASAR_LLM_CHAT_PARAMETERS=old-value\n# END QUASAR SETUP\n')
        values = self.root / "parameters"
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_FAIL_COMPAT="yes",
                                         QUASAR_TEST_PARAMETERS_TRACE=str(values)),
                                input=("y\nhttps://first.example.invalid/v1\n\n1\n2\ny\n"
                                       "https://second.example.invalid/v1\n\n2\n2\n"),
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.trace.read_text().splitlines(),
                         ["-list-models", "-probe-llm-compat", "-list-models", "-check-config", "-check-backends", "install"])
        self.assertTrue(all(value == "{}" for value in values.read_text().splitlines()))
        output = subprocess.check_output([
            "sh", "-c", '. "$1"; printf "%s\\n" "$QUASAR_LLM_API" "$QUASAR_LLM_CHAT_PARAMETERS"',
            "sh", str(self.state / "runtime.env"),
        ], text=True)
        self.assertEqual(output.splitlines(), ["responses", "{}"])

    def test_cancelling_after_successful_probe_restores_old_parameters(self):
        self.state.mkdir()
        config = '{"mode":"observe","llm_chat_parameters":{"temperature":0.5}}'
        runtime = 'KEEP_FIXTURE=yes\nQUASAR_LLM_CHAT_PARAMETERS=\'{"temperature":0.5}\'\n'
        (self.state / "config.json").write_text(config)
        (self.state / "runtime.env").write_text(runtime)
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_FAIL_BACKEND_ONCE="yes",
                                         QUASAR_TEST_COMPAT_PARAMETERS='{"chat_template_kwargs":null}'),
                                input="y\nhttps://llm.example.invalid/v1\n\n1\n2\n0\n",
                                capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.trace.read_text().splitlines(),
                         ["-list-models", "-probe-llm-compat", "-check-config", "-check-backends"])
        self.assertEqual((self.state / "config.json").read_text(), config)
        self.assertEqual((self.state / "runtime.env").read_text(), runtime)

    def test_update_without_reconfiguration_keeps_parameters_and_skips_probe(self):
        self.state.mkdir()
        config = '{"mode":"observe","llm_chat_parameters":{"chat_template_kwargs":null}}'
        runtime = 'QUASAR_LLM_CHAT_PARAMETERS=\'{"chat_template_kwargs":null}\'\nKEEP_FIXTURE=yes\n'
        (self.state / "config.json").write_text(config)
        (self.state / "runtime.env").write_text(runtime)
        (self.state / "endpoint-backup.json").write_text("existing Alice connection")
        app = self.state / "app"
        app.mkdir()
        (app / "control.sh").write_text('#!/bin/sh\nprintf "control %s\\n" "$*" >> "$QUASAR_TEST_TRACE"\n')
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=self.env, input="n\n", capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.trace.read_text().splitlines(), ["install", "control check-backends"])
        self.assertEqual((self.state / "config.json").read_text(), config)
        self.assertEqual((self.state / "runtime.env").read_text(), runtime)

    def test_model_selection_rejects_invalid_numbers_without_refetching(self):
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=self.env, input="chatgpt\nname\n0\n3\n2\n\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        output = subprocess.check_output([
            "sh", "-c", '. "$1"; printf "%s" "$QUASAR_LLM_MODEL"',
            "sh", str(self.state / "runtime.env"),
        ], text=True)
        self.assertEqual(output, "fixture-model")
        self.assertEqual(self.trace.read_text().splitlines().count("-list-models"), 1)

    def test_failed_or_empty_catalog_does_not_install(self):
        for setting in ("QUASAR_TEST_FAIL_MODELS", "QUASAR_TEST_EMPTY_MODELS"):
            with self.subTest(setting=setting):
                self.trace.unlink(missing_ok=True)
                result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                        env=dict(self.env, **{setting: "yes"}),
                                        input="https://llm.example.invalid/v1\n\n1\nn\n",
                                        capture_output=True, text=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(self.trace.read_text().splitlines(), ["-list-models"])
                self.assertNotIn("Номер модели:", result.stdout)
                self.assertFalse((self.state / "runtime.env").exists())
                self.assertFalse((self.state / "config.json").exists())

    def test_catalog_failure_restores_existing_configuration(self):
        self.state.mkdir()
        config = '{"mode":"observe","llm_model":"old-model"}'
        runtime = "KEEP_FIXTURE=yes\nQUASAR_LLM_MODEL=old-model\n"
        (self.state / "config.json").write_text(config)
        (self.state / "runtime.env").write_text(runtime)
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_FAIL_MODELS="yes"),
                                input="y\nhttps://llm.example.invalid/v1\n\n1\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual((self.state / "config.json").read_text(), config)
        self.assertEqual((self.state / "runtime.env").read_text(), runtime)

    def test_changing_llm_keeps_home_url_without_asking_again(self):
        self.state.mkdir()
        (self.state / "config.json").write_text('{"mode":"observe"}')
        (self.state / "endpoint-backup.json").write_text("existing Alice connection")
        home_url = "https://home.example.invalid/mcp/fixture'\"\\$`secret"
        escaped = home_url.replace("\\", "\\\\").replace('"', '\\"').replace("$", "\\$").replace("`", "\\`")
        runtime = ('KEEP_FIXTURE=yes\n# BEGIN QUASAR SETUP\n'
                   'QUASAR_LLM_MODEL="old-model"\n'
                   f'QUASAR_HOME_MCP_URL="{escaped}"\n'
                   '# END QUASAR SETUP\n')
        (self.state / "runtime.env").write_text(runtime)
        home_trace = self.root / "home-values"
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_HOME_TRACE=str(home_trace)),
                                input="y\nhttps://llm.example.invalid/v1\n\n1\n2\n",
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("URL Home Assistant MCP", result.stdout)
        output = subprocess.check_output([
            "sh", "-c", '. "$1"; printf "%s\\n" "$QUASAR_HOME_MCP_URL" "$QUASAR_LLM_MODEL" "$KEEP_FIXTURE"',
            "sh", str(self.state / "runtime.env"),
        ], text=True)
        self.assertEqual(output.splitlines(), [home_url, "fixture-model", "yes"])
        self.assertTrue(all(value == home_url for value in home_trace.read_text().splitlines()))

    def test_changing_llm_keeps_external_and_inherited_home_settings(self):
        cases = (
            ('QUASAR_HOME_MCP_URL="https://home.example.invalid/outside"\n', "", "https://home.example.invalid/outside"),
            ('# BEGIN QUASAR SETUP\nQUASAR_HOME_MCP_URL="https://home.example.invalid/old"\n'
             '# END QUASAR SETUP\nQUASAR_HOME_MCP_URL="https://home.example.invalid/current"\n',
             "", "https://home.example.invalid/current"),
            ("KEEP_FIXTURE=yes\n", "https://home.example.invalid/inherited", "https://home.example.invalid/inherited"),
            ("", "", ""),
        )
        for runtime, inherited, expected in cases:
            with self.subTest(runtime=runtime, inherited=inherited):
                shutil.rmtree(self.state, ignore_errors=True)
                self.state.mkdir()
                (self.state / "config.json").write_text('{"mode":"observe"}')
                (self.state / "endpoint-backup.json").write_text("existing Alice connection")
                if runtime:
                    (self.state / "runtime.env").write_text(runtime)
                home_trace = self.root / "home-values"
                home_trace.unlink(missing_ok=True)
                env = dict(self.env, QUASAR_HOME_MCP_URL=inherited,
                           QUASAR_TEST_HOME_TRACE=str(home_trace))
                result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                        env=env,
                                        input="y\nhttps://llm.example.invalid/v1\n\n1\n2\n",
                                        capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertNotIn("URL Home Assistant MCP", result.stdout)
                self.assertTrue(all(value == expected for value in home_trace.read_text().splitlines()))
                saved = (self.state / "runtime.env").read_text()
                if "QUASAR_HOME_MCP_URL=" not in runtime:
                    self.assertNotIn("QUASAR_HOME_MCP_URL=", saved)
                output = subprocess.check_output([
                    "sh", "-c", '. "$1"; printf "%s" "$QUASAR_HOME_MCP_URL"',
                    "sh", str(self.state / "runtime.env"),
                ], env=env, text=True)
                self.assertEqual(output, expected)

    def test_backend_retry_keeps_home_url_without_asking_again(self):
        home_url = "https://home.example.invalid/first-input"
        home_trace = self.root / "home-values"
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_TEST_FAIL_BACKEND_ONCE="yes",
                                         QUASAR_TEST_HOME_TRACE=str(home_trace)),
                                input=(f"https://llm.example.invalid/v1\n\n1\n2\n{home_url}\ny\n"
                                       "https://llm.example.invalid/v1\n\n1\n2\nn\n"),
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(result.stdout.count("URL Home Assistant MCP"), 1)
        self.assertIn("backend temporarily unavailable", result.stdout + result.stderr)
        values = home_trace.read_text().splitlines()
        self.assertEqual(values[:2], ["", ""])
        self.assertTrue(all(value == home_url for value in values[2:]))

    def test_failed_backend_can_correct_only_home_url(self):
        wrong_url = "https://home.example.invalid/typo"
        corrected_url = "https://home.example.invalid/corrected"
        for existing in (False, True):
            with self.subTest(existing=existing):
                shutil.rmtree(self.state, ignore_errors=True)
                self.trace.unlink(missing_ok=True)
                home_trace = self.root / "home-values"
                home_trace.unlink(missing_ok=True)
                if existing:
                    self.state.mkdir()
                    (self.state / "config.json").write_text('{"mode":"observe"}')
                    (self.state / "endpoint-backup.json").write_text("existing Alice connection")
                    (self.state / "runtime.env").write_text(
                        f'# BEGIN QUASAR SETUP\nQUASAR_HOME_MCP_URL="{wrong_url}"\n# END QUASAR SETUP\n')
                answers = ("y\n" if existing else "") + "https://llm.example.invalid/v1\n\n1\n2\n"
                if not existing:
                    answers += wrong_url + "\n"
                answers += f"2\n{corrected_url}\n" + ("" if existing else "n\n")
                result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                        env=dict(self.env, QUASAR_TEST_FAIL_BACKEND_ONCE="yes",
                                                 QUASAR_TEST_HOME_TRACE=str(home_trace),
                                                 QUASAR_TEST_COMPAT_PARAMETERS='{"chat_template_kwargs":null}'),
                                        input=answers, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                calls = self.trace.read_text().splitlines()
                self.assertEqual(calls.count("-list-models"), 1)
                self.assertEqual(calls.count("-check-backends"), 2)
                self.assertEqual(result.stdout.count("URL Home Assistant MCP"), 1 if existing else 2)
                values = home_trace.read_text().splitlines()
                self.assertIn(wrong_url, values)
                self.assertEqual(values[-1], corrected_url)
                output = subprocess.check_output([
                    "sh", "-c", '. "$1"; printf "%s\\n" "$QUASAR_HOME_MCP_URL" "$QUASAR_LLM_MODEL" "$QUASAR_LLM_CHAT_PARAMETERS"',
                    "sh", str(self.state / "runtime.env"),
                ], text=True)
                self.assertEqual(output.splitlines(),
                                 [corrected_url, "fixture-model", '{"chat_template_kwargs":null}'])

    def test_subscription_setup_shows_concrete_tunnel_and_hides_status_json(self):
        tunnel = "ssh -N -L 1455:127.0.0.1:1455 -p 2222 root@192.0.2.24"
        result = subprocess.run(["sh", str(self.package / "setup.sh")],
                                env=dict(self.env, QUASAR_SSH_TUNNEL=tunnel),
                                input="chatgpt\n2\n\nn\n",
                                capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(tunnel, result.stdout)
        self.assertNotIn("SSH_PORT", result.stdout)
        self.assertNotIn("SPEAKER_IP", result.stdout)
        self.assertIn("https://auth.example.invalid/authorize?state=fixture", result.stdout)
        self.assertNotIn('"signed_in"', result.stdout)
        self.assertNotIn("fixture@example.invalid", result.stdout)
        self.assertNotIn("\x1b[", result.stdout)

    def test_subscription_url_arrives_before_sign_in_completes_when_piped(self):
        marker = self.root / "complete-login"
        process = subprocess.Popen(["sh", str(self.package / "setup.sh")],
                                   env=dict(self.env, QUASAR_TEST_AUTH_WAIT_FILE=str(marker)),
                                   stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.STDOUT)
        process.stdin.write(b"chatgpt\n2\n\nn\n")
        process.stdin.close()
        process.stdin = None
        output = bytearray()
        url = b"https://auth.example.invalid/authorize?state=fixture"
        deadline = time.monotonic() + 3
        try:
            while url not in output and time.monotonic() < deadline:
                if select.select([process.stdout], [], [], max(0, deadline - time.monotonic()))[0]:
                    chunk = os.read(process.stdout.fileno(), 65536)
                    if not chunk:
                        break
                    output.extend(chunk)
            self.assertIn(url, output, output.decode(errors="replace"))
            self.assertIsNone(process.poll())
            self.assertNotIn(b'"signed_in"', output)
        finally:
            # Release the auth fixture even if the URL was buffered or an assertion failed.
            marker.touch()
            try:
                remaining, _ = process.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.communicate(timeout=5)
                raise
            output.extend(remaining)
        self.assertEqual(process.returncode, 0, output.decode(errors="replace"))
        self.assertNotIn(b'"signed_in"', output)

    def test_terminal_echoes_home_url_but_hides_api_key(self):
        master, slave = pty.openpty()
        process = subprocess.Popen(["sh", str(self.package / "setup.sh")],
                                   env=dict(self.env, TERM="xterm", NO_COLOR="1"),
                                   stdin=slave, stdout=slave, stderr=slave)
        os.close(slave)
        output = bytearray()
        cursor = 0

        def reply_after(fragment, answer, echo=None):
            nonlocal cursor
            deadline = time.monotonic() + 5
            needle = fragment.encode()
            while needle not in output[cursor:]:
                remaining = deadline - time.monotonic()
                self.assertGreater(remaining, 0, bytes(output).decode(errors="replace"))
                if select.select([master], [], [], remaining)[0]:
                    output.extend(os.read(master, 65536))
            # Drain the prompt before sending input, including its styling suffix.
            while select.select([master], [], [], 0.05)[0]:
                output.extend(os.read(master, 65536))
            cursor = len(output)
            if echo is not None:
                self.assertEqual(bool(termios.tcgetattr(master)[3] & termios.ECHO), echo,
                                 bytes(output).decode(errors="replace"))
            os.write(master, (answer + "\n").encode())

        try:
            reply_after("Адрес API", "https://llm.example.invalid/v1")
            reply_after("Ключ API", "fixture-api-key-hidden", echo=False)
            reply_after("Chat Completions", "1")
            reply_after("Номер модели", "2")
            home_url = "https://home.example.invalid/visible-input"
            reply_after("URL Home Assistant MCP", home_url, echo=True)
            reply_after("Подключить Алису", "n")
            deadline = time.monotonic() + 5
            while process.poll() is None or select.select([master], [], [], 0)[0]:
                self.assertLess(time.monotonic(), deadline, output.decode(errors="replace"))
                if select.select([master], [], [], 0.1)[0]:
                    try:
                        chunk = os.read(master, 65536)
                    except OSError:
                        break
                    if not chunk:
                        break
                    output.extend(chunk)
            self.assertEqual(process.wait(timeout=5), 0, output.decode(errors="replace"))
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
            os.close(master)
        text = output.decode()
        self.assertIn(home_url, text)
        self.assertNotIn("fixture-api-key-hidden", text)
        self.assertNotIn("\x1b[", text)

    def test_control_forwards_authentication_actions(self):
        self.state.mkdir()
        for action in ("login", "status", "models", "logout"):
            with self.subTest(action=action):
                result = subprocess.run(["sh", str(self.package / "control.sh"), "chatgpt-" + action],
                                        env=self.env, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.trace.read_text().splitlines(),
                         ["-chatgpt-auth " + action for action in ("login", "status", "models", "logout")])


class HostBootstrapTest(unittest.TestCase):
    def test_tunnel_command_keeps_target_and_user_ssh_options(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            release = root / "release"
            release.mkdir()
            setup = root / "setup.sh"
            setup.write_text('#!/bin/sh\nprintf "%s" "${QUASAR_SSH_TUNNEL:-}" > "$QUASAR_TEST_TUNNEL"\n')
            archive = release / "quasar-bridge-linux-arm64.tar.gz"
            with tarfile.open(archive, "w:gz") as tar:
                tar.add(setup, arcname="setup.sh")
            archive.with_suffix(".gz.sha256").write_text(
                f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n")

            class Handler(http.server.SimpleHTTPRequestHandler):
                def __init__(self, *args, **kwargs):
                    super().__init__(*args, directory=str(release), **kwargs)

                def log_message(self, *args):
                    pass

            server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            shim = root / "shim"
            shim.mkdir()
            ssh = shim / "ssh"
            ssh.write_text('''#!/usr/bin/env python3
import os
import subprocess
import sys
if "-O" in sys.argv:
    sys.exit(0)
command = sys.argv[-1]
if command == "uname -m":
    print("aarch64")
    sys.exit(0)
sys.exit(subprocess.call(["sh", "-c", command], env=os.environ))
''')
            ssh.chmod(0o755)
            tunnel_file = root / "tunnel-command"
            options = ["-p", "2222", "-i", "/tmp/identity 'with spaces'", "-o", "ServerAliveInterval=30"]
            try:
                result = subprocess.run(["sh", str(ROOT / "install.sh"), "root@fixture", *options],
                                        env=dict(os.environ,
                                                 PATH=str(shim) + os.pathsep + os.environ["PATH"],
                                                 QUASAR_RELEASE_URL=f"http://127.0.0.1:{server.server_port}",
                                                 QUASAR_TEST_TUNNEL=str(tunnel_file)),
                                        input="", capture_output=True, text=True, timeout=10)
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=5)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            tunnel = shlex.split(tunnel_file.read_text())
            self.assertEqual(tunnel, ["ssh", "-N", "-L", "1455:127.0.0.1:1455", *options, "root@fixture"])
            self.assertNotIn("ControlPath", tunnel_file.read_text())
            self.assertNotIn("ControlMaster", tunnel_file.read_text())
            self.assertNotIn("\x1b[", result.stdout)


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
        for name in ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES", "SOURCE.md"):
            shutil.copy2(ROOT / name, self.package / name)
        shutil.copytree(ROOT / "docs/licenses", self.package / "docs/licenses")
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

    def control(self, *args, **kwargs):
        return self.run_script(self.state / "app/control.sh", *args, **kwargs)

    def install(self, **kwargs):
        return self.run_script(self.package / "install.sh", **kwargs)

    def stop(self):
        if (self.state / "app/control.sh").exists():
            self.control("stop")

    def test_install_update_and_uninstall_keep_state_and_boot_independently(self):
        self.install()
        self.assertEqual(sorted(p.name for p in (self.state / "app/bin").iterdir()), ["quasar-bridge"])
        for name in ("SOURCE.md", "docs/licenses/go-aac-LGPL-3.0.txt",
                     "docs/licenses/go-aac-GPL-3.0.txt"):
            self.assertEqual((self.state / "app" / name).read_bytes(),
                             (self.package / name).read_bytes())
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
        replacement = (self.package / "bin/quasar-bridge").read_bytes() + b"updated-single-file-fixture"
        (self.package / "bin/quasar-bridge").write_bytes(replacement)
        self.install()
        self.assertEqual((self.state / "app/bin/quasar-bridge").read_bytes(), replacement)
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

    def test_wake_enable_and_disable_edit_only_the_second_slot(self):
        self.install()
        firmware = self.root / "firmware"
        models = firmware / "spotter_models"
        for word in ("alisa", "yandex"):
            (models / "activation" / word).mkdir(parents=True)
            (models / "activation" / word / "acoustic_model.nnet").write_text(word)
        # Factory formatting: indented, spaces around colons; numbers must survive.
        config_text = ('{\n   "common" : { "rate" : 0.14999999999999999 },\n'
                       '   "aliced" : {\n      "keep" : 42\n   }\n}\n')
        (firmware / "quasar.cfg").write_text(config_text)
        (firmware / "maind").write_bytes(b"\x7fELF....customActivation....")
        (firmware / "user_config.dat").write_text('xx{"account_config":{"spotter":"alisa"}}')
        self.env.update(QUASAR_DEVICE_CONFIG=str(firmware / "quasar.cfg"),
                        QUASAR_DEVICE_MODELS=str(models), QUASAR_DEVICE_MAIND=str(firmware / "maind"),
                        QUASAR_DEVICE_USER_CONFIG=str(firmware / "user_config.dat"))
        self.assertIn("enabled: no", self.control("wake", "status").stdout)
        self.control("wake", "enable")  # the app has "alisa", so the second word is "yandex"
        changed = json.loads((firmware / "quasar.cfg").read_text())
        self.assertEqual(changed["aliced"]["customActivation"], {"forceEnable": True, "forcePhrase": "yandex"})
        self.assertIn("0.14999999999999999", (firmware / "quasar.cfg").read_text())
        slot = models / "custom_activation"
        self.assertEqual((slot / "acoustic_model.nnet").read_text(), "yandex")
        self.assertTrue((slot / ".second-wake-word").exists())
        self.control("wake", "enable", ok=False)  # already enabled
        self.run_script(self.state / "app/uninstall.sh", ok=False)  # must be disabled first
        self.control("wake", "disable")
        self.assertEqual((firmware / "quasar.cfg").read_text(), config_text)
        self.assertFalse(slot.exists())
        self.control("wake", "bogus", ok=False)

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

    def backend_url(self, reject_template=False):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"object":"list","data":[{"id":"fixture-model","object":"model"}]}')

            def do_POST(self):
                request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                if reject_template and "chat_template_kwargs" in request:
                    self.send_response(400)
                    self.send_header("Content-Type", "application/json")
                    self.end_headers()
                    self.wfile.write(b'{"error":{"message":"Unknown name chat_template_kwargs", "status":"INVALID_ARGUMENT"}}')
                    return
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                content = '{"ok":true}' if "response_format" in request else "ok"
                self.wfile.write(json.dumps({"choices": [{"message": {"content": content}}]}).encode())

            def log_message(self, *args):
                pass

        return self.serve(Handler) + "/v1"

    def test_setup_probes_restricted_chat_api_and_saves_runtime_parameters(self):
        url = self.backend_url(reject_template=True)
        result = self.run_script(self.package / "setup.sh",
                                 input_text=f"{url}\nfixture-key\n1\n1\n\nn\n")
        self.control("status")
        output = subprocess.check_output([
            "sh", "-c", '. "$1"; printf "%s" "$QUASAR_LLM_CHAT_PARAMETERS"',
            "sh", str(self.state / "runtime.env"),
        ], text=True)
        self.assertEqual(json.loads(output), {"chat_template_kwargs": None})
        self.assertIn("Совместимые параметры API подобраны", result.stdout)

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
                                 input_text=f"{url}\n{key}\n1\n1\n\ny\nn\n")
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
            def do_GET(self):
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(b'{"object":"list","data":[{"id":"fixture-model","object":"model"}]}')

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
                        input_text=f"y\n{url}\nfixture-key\n1\n1\nn\n", ok=False)
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
