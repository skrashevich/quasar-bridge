"""Check that image preparation failures remain visible in CI logs."""
import contextlib
import io
from pathlib import Path
import subprocess
import tempfile
import unittest

import prepare


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


if __name__ == "__main__":
    unittest.main()
