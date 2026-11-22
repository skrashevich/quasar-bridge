"""Check that image preparation failures remain visible in CI logs."""
import contextlib
import hashlib
import io
from pathlib import Path
import subprocess
import tempfile
import unittest

import prepare
from firmware import ARCHES, CORE, DATASET_REV, EXTENDED, UNSUPPORTED, Firmware, matrix, summary


class PrepareDiagnosticsTest(unittest.TestCase):
    def test_reports_emulator_stderr_and_keeps_failing_exit_status(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            emulator = root / "kuyase"
            emulator.write_text("#!/bin/sh\necho 'GLIBC_2.43 not found' >&2\nexit 1\n")
            emulator.chmod(0o755)
            console = io.StringIO()
            with contextlib.redirect_stdout(console):
                with self.assertRaises(subprocess.CalledProcessError):
                    prepare.prepare_image(root, "station2", root / "rootfs.tar", root / "prepare.log")
            self.assertIn("GLIBC_2.43 not found", console.getvalue())
            self.assertIn("GLIBC_2.43 not found", (root / "prepare.log").read_text())


class FirmwareMatrixTest(unittest.TestCase):
    def test_mandatory_jobs_stay_exactly_the_same(self):
        expected = [
            {"board": "yandexmini", "arch": "arm64"},
            {"board": "yandexmini", "arch": "armv7"},
            {"board": "station2", "arch": "arm64"},
            {"board": "station2", "arch": "armv7"},
        ]
        for event, ref in (("push", "refs/heads/topic"),
                           ("workflow_call", "refs/heads/main")):
            self.assertEqual(matrix(event, ref)["include"], expected)

    def test_extended_pull_request_main_and_manual(self):
        expected = [{"board": board, "arch": arch}
                    for board in (*CORE, *EXTENDED) for arch in ARCHES]
        for event, ref in (("pull_request", "refs/pull/42/merge"),
                           ("push", "refs/heads/main"),
                           ("workflow_dispatch", "refs/heads/topic")):
            self.assertEqual(matrix(event, ref)["include"], expected)
        for spec in EXTENDED.values():
            self.assertIn(f"/resolve/{DATASET_REV}/", spec.url)
            self.assertRegex(spec.ota_sha256, r"^[0-9a-f]{64}$")
            self.assertRegex(spec.member_sha256, r"^[0-9a-f]{64}$")
        markdown = summary(matrix("push", "refs/heads/topic"))
        for label in (*EXTENDED, *UNSUPPORTED):
            self.assertIn(f"`{label}`", markdown)
        self.assertEqual(markdown.count("не запускается на этом событии"), len(EXTENDED))


def cpio_entry(name, body):
    name = name.encode() + b"\0"
    fields = [1, 0o100644, 0, 0, 1, 0, len(body), 0, 0, 0, 0, len(name), 0]
    data = b"070701" + "".join(f"{value:08x}" for value in fields).encode() + name
    return data + b"\0" * (-len(data) % 4) + body + b"\0" * (-len(body) % 4)


class FirmwareExtractionTest(unittest.TestCase):
    def test_cpio_validates_container_and_selected_member(self):
        for member in ("rootfs.ubifs", "rootfs.ext4.gz"):
            with self.subTest(member=member), tempfile.TemporaryDirectory() as tmp:
                payload = b"fake root filesystem"
                data = cpio_entry("ignored", b"not a filesystem") + cpio_entry(member, payload)
                ota = Path(tmp) / "ota"
                ota.write_bytes(data)
                digest = hashlib.sha256
                spec = Firmware("unused", digest(data).hexdigest(), digest(payload).hexdigest(), member=member)
                self.assertEqual(prepare.read_rootfs(ota, spec), payload)
                bad_member = Firmware("unused", digest(data).hexdigest(), digest(b"wrong").hexdigest(), member=member)
                with self.assertRaisesRegex(ValueError, "SHA-256"):
                    prepare.read_rootfs(ota, bad_member)
                ota.write_bytes(data + b"corrupt")
                with self.assertRaisesRegex(ValueError, "SHA-256"):
                    prepare.read_rootfs(ota, spec)


if __name__ == "__main__":
    unittest.main()
