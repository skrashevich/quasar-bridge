#!/usr/bin/env python3
"""Exercise the public PR bootstrap without a speaker or build toolchain."""
import copy
import hashlib
import http.server
import io
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tarfile
import tempfile
import threading
import unittest
import zipfile

ROOT = Path(__file__).resolve().parent.parent
HEAD = "a" * 40
OTHER = "b" * 40


class PRInstallTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="quasar-pr-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.archive_name = "quasar-bridge-linux-arm64.tar.gz"
        data = io.BytesIO()
        setup = b'''#!/bin/sh
if test -f "$PR_TEST_CONFIG"; then
 printf 'update\\n' >> "$PR_TEST_SETUP"
else
 printf 'initial configuration\\n' > "$PR_TEST_CONFIG"
 printf 'install\\n' >> "$PR_TEST_SETUP"
fi
printf '%s\\n' "${QUASAR_SSH_TUNNEL:-}" >> "$PR_TEST_TUNNEL"
'''
        with tarfile.open(fileobj=data, mode="w:gz") as tar:
            entry = tarfile.TarInfo("setup.sh")
            entry.size = len(setup)
            tar.addfile(entry, io.BytesIO(setup))
        self.archive = data.getvalue()
        self.checksum = hashlib.sha256(self.archive).hexdigest()
        self.zip = self.make_zip()
        self.pr = {"head": {"sha": HEAD}, "number": 30,
                   "body": 'Quotes " and braces {} and escaped newline\nЮникод'}
        self.runs = {"workflow_runs": [self.run_info()]}
        self.artifacts = {"artifacts": [{
            "name": "speaker-release-arm64", "id": 200, "expired": False,
            "digest": "sha256:" + hashlib.sha256(self.zip).hexdigest(),
            "workflow_run": {"id": 100, "head_sha": HEAD},
        }]}
        self.queries = []
        self.change_head = False
        self.compact = False
        self.pr_response = None
        self.api_failure = False
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                outer.queries.append(self.path)
                if self.path == "/repos/skrashevich/quasar-bridge/pulls/30":
                    if outer.api_failure:
                        self.send_error(403, "API rate limit exceeded")
                        return
                    if outer.pr_response is not None:
                        self.send_response(200)
                        self.end_headers()
                        self.wfile.write(outer.pr_response)
                        return
                    obj = copy.deepcopy(outer.pr)
                    if outer.change_head and outer.queries.count(self.path) > 1:
                        obj["head"]["sha"] = OTHER
                elif self.path.startswith("/repos/skrashevich/quasar-bridge/actions/workflows/ci.yml/runs?"):
                    obj = outer.runs
                elif self.path == "/repos/skrashevich/quasar-bridge/actions/runs/100/artifacts?per_page=100":
                    obj = outer.artifacts
                elif self.path == "/skrashevich/quasar-bridge/actions/artifacts/200.zip":
                    self.send_response(200)
                    self.end_headers()
                    self.wfile.write(outer.zip)
                    return
                else:
                    self.send_error(404)
                    return
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps(obj, indent=None if outer.compact else 2).encode())

            def log_message(self, *args):
                pass

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.stop_server)
        shim = self.root / "shim"
        shim.mkdir()
        self.shim = shim
        real_curl = shlex.quote(shutil.which("curl"))
        (shim / "curl").write_text(f'''#!/bin/sh
out=
url=
while test "$#" -gt 0; do
 case "$1" in
  -o) out=$2; shift 2 ;;
  https://api.github.com/*) url=${{1#https://api.github.com}}; shift ;;
  https://nightly.link/*) url=${{1#https://nightly.link}}; shift ;;
  *) shift ;;
 esac
done
test -n "$url" || exit 90
exec {real_curl} -fsSL --connect-timeout 5 -o "$out" "$PR_TEST_HTTP$url"
''')
        (shim / "ssh").write_text('''#!/bin/sh
for argument do command=$argument; done
for argument do if test "$argument" = -O; then exit 0; fi; done
printf '%s\\n' "$command" >> "$PR_TEST_SSH"
if test "$command" = 'uname -m'; then printf '%s\\n' "${PR_TEST_ARCH:-aarch64}"; exit 0; fi
exec sh -c "$command"
''')
        for name in ("go", "python", "python3", "jq", "gh"):
            (shim / name).write_text('#!/bin/sh\nprintf "%s\\n" "$0" >> "$PR_TEST_FORBIDDEN"\nexit 99\n')
        for path in shim.iterdir():
            path.chmod(0o755)
        self.env = dict(os.environ, PATH=str(shim) + os.pathsep + os.environ["PATH"],
                        PR_TEST_HTTP=f"http://127.0.0.1:{self.server.server_port}",
                        PR_TEST_CONFIG=str(self.root / "config"),
                        PR_TEST_SETUP=str(self.root / "setup"),
                        PR_TEST_TUNNEL=str(self.root / "tunnel"),
                        PR_TEST_SSH=str(self.root / "ssh-log"),
                        PR_TEST_FORBIDDEN=str(self.root / "forbidden"), NO_COLOR="1")
        self.env.pop("QUASAR_RELEASE_URL", None)

    def stop_server(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def make_zip(self, checksum=None, extra=False):
        output = io.BytesIO()
        with zipfile.ZipFile(output, "w", zipfile.ZIP_DEFLATED) as archive:
            archive.writestr(self.archive_name, self.archive)
            archive.writestr(self.archive_name + ".sha256",
                             f"{checksum or self.checksum}  {self.archive_name}\n")
            if extra:
                archive.writestr("../unsafe", "must never extract this")
        return output.getvalue()

    def run_info(self, **updates):
        obj = {"id": 100, "head_sha": HEAD, "event": "pull_request",
               "path": ".github/workflows/ci.yml", "status": "completed",
               "conclusion": "success", "pull_requests": [{"number": 30}]}
        obj.update(updates)
        return obj

    def install(self, args=None):
        result = subprocess.run(["sh", str(ROOT / "install.sh"), *(args or ["--pr", "30", "root@fixture", "-p", "2222"])],
                                env=self.env, input="", capture_output=True, text=True, timeout=12)
        self.assertFalse((self.root / "forbidden").exists(), "bootstrap invoked a build tool or JSON dependency")
        return result

    def assert_rejected(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.root / "setup").exists())
        if (self.root / "ssh-log").exists():
            self.assertEqual((self.root / "ssh-log").read_text(), "uname -m\n", "device changed before validation")

    def test_install_and_update_without_compilers(self):
        first = self.install()
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        (self.root / "config").write_text("existing secret configuration")
        second = self.install()
        self.assertEqual(second.returncode, 0, second.stdout + second.stderr)
        self.assertEqual((self.root / "setup").read_text(), "install\nupdate\n")
        self.assertEqual((self.root / "config").read_text(), "existing secret configuration")
        self.assertIn("-p 2222 root@fixture", (self.root / "tunnel").read_text())

    def test_compact_json_and_escaped_unrelated_strings(self):
        self.compact = True
        self.pr = dict(reversed(list(self.pr.items())))
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_latest_matching_run_must_succeed(self):
        for status, conclusion in [("in_progress", None), ("completed", "failure"), ("completed", "cancelled")]:
            with self.subTest(status=status, conclusion=conclusion):
                self.runs["workflow_runs"] = [self.run_info(id=101, status=status, conclusion=conclusion), self.run_info()]
                self.assert_rejected(self.install())
                (self.root / "ssh-log").unlink(missing_ok=True)

    def test_unrelated_newer_run_ignored(self):
        self.runs["workflow_runs"] = [self.run_info(id=101, path=".github/workflows/other.yml"), self.run_info()]
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_wrong_head_run_rejected(self):
        self.runs["workflow_runs"][0]["head_sha"] = OTHER
        self.assert_rejected(self.install())

    def test_wrong_pr_run_or_missing_run_rejected(self):
        for runs in [[], [self.run_info(pull_requests=[{"number": 31}])]]:
            with self.subTest(runs=runs):
                self.runs["workflow_runs"] = runs
                self.assert_rejected(self.install())
                (self.root / "ssh-log").unlink(missing_ok=True)

    def test_github_api_failure_rejected(self):
        self.api_failure = True
        self.assert_rejected(self.install())

    def test_malformed_and_duplicate_json_rejected(self):
        for response in [b'{"head":', b'{"head":{"sha":"' + HEAD.encode() + b'","sha":"' + OTHER.encode() + b'"}}',
                         b'{"head":{"sha":"' + HEAD.encode() + b'"}} trailing']:
            with self.subTest(response=response):
                self.pr_response = response
                self.assert_rejected(self.install())
                (self.root / "ssh-log").unlink(missing_ok=True)

    def test_unicode_escape_in_json_key(self):
        self.pr_response = b'{"h\\u0065ad":{"sha":"' + HEAD.encode() + b'"}}'
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_or_expired_or_ambiguous_artifact_rejected(self):
        original = copy.deepcopy(self.artifacts["artifacts"])
        for artifacts in [[], [dict(original[0], expired=True)], original * 2]:
            with self.subTest(artifacts=artifacts):
                self.artifacts["artifacts"] = artifacts
                self.assert_rejected(self.install())
                (self.root / "ssh-log").unlink(missing_ok=True)

    def test_wrong_artifact_run_or_head_rejected(self):
        for key, value in [("id", 101), ("head_sha", OTHER)]:
            with self.subTest(key=key):
                self.artifacts["artifacts"][0]["workflow_run"] = {"id": 100, "head_sha": HEAD, key: value}
                self.assert_rejected(self.install())
                (self.root / "ssh-log").unlink(missing_ok=True)

    def test_missing_or_wrong_zip_digest_rejected(self):
        for digest in [None, "sha256:" + "0" * 64, "sha1:" + "a" * 40]:
            with self.subTest(digest=digest):
                self.artifacts["artifacts"][0]["digest"] = digest
                self.assert_rejected(self.install())
                (self.root / "ssh-log").unlink(missing_ok=True)

    def test_corrupt_zip_rejected_even_with_matching_digest(self):
        self.zip = b"not a ZIP"
        self.artifacts["artifacts"][0]["digest"] = "sha256:" + hashlib.sha256(self.zip).hexdigest()
        self.assert_rejected(self.install())

    def test_wrong_tar_checksum_rejected(self):
        self.zip = self.make_zip(checksum="0" * 64)
        self.artifacts["artifacts"][0]["digest"] = "sha256:" + hashlib.sha256(self.zip).hexdigest()
        self.assert_rejected(self.install())

    def test_zip_does_not_extract_unexpected_paths(self):
        self.zip = self.make_zip(extra=True)
        self.artifacts["artifacts"][0]["digest"] = "sha256:" + hashlib.sha256(self.zip).hexdigest()
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertFalse((self.root / "unsafe").exists())

    def test_changed_pr_head_rejected_before_device_changes(self):
        self.change_head = True
        self.assert_rejected(self.install())

    def test_architecture_specific_artifact(self):
        self.env["PR_TEST_ARCH"] = "armv7l"
        self.assert_rejected(self.install())
        self.assertFalse(any(path.endswith("/200.zip") for path in self.queries))

    def test_armv7_success(self):
        self.env["PR_TEST_ARCH"] = "armv7l"
        self.archive_name = "quasar-bridge-linux-armv7.tar.gz"
        self.zip = self.make_zip()
        artifact = self.artifacts["artifacts"][0]
        artifact["name"] = "speaker-release-armv7"
        artifact["digest"] = "sha256:" + hashlib.sha256(self.zip).hexdigest()
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_help_does_not_contact_device(self):
        result = self.install(["--help"])
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("--pr", result.stdout)
        self.assertFalse((self.root / "ssh-log").exists())

    def test_argument_validation_and_release_override(self):
        for args in [["--pr"], ["--pr", "0"], ["--pr", "-1"], ["--pr", "abc"], ["--pr", "30x"], ["--pr", "01"]]:
            with self.subTest(args=args):
                result = self.install(args)
                self.assert_rejected(result)
        self.env["QUASAR_RELEASE_URL"] = "https://example.invalid"
        self.assert_rejected(self.install())


if __name__ == "__main__":
    unittest.main()
