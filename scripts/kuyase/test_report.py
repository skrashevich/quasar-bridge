"""A failed scenario must survive later passes in all CI report formats."""
import contextlib
import io
import json
from pathlib import Path
import shlex
import tempfile
import unittest
from unittest.mock import patch
import xml.etree.ElementTree as ET

from report import Report
import run


class ReportTest(unittest.TestCase):
    def test_failure_survives_later_pass_and_is_visible_in_junit(self):
        with tempfile.TemporaryDirectory() as tmp, contextlib.redirect_stdout(io.StringIO()):
            report = Report("station2", tmp)
            def fail():
                raise RuntimeError("wrong backend <llm>")
            self.assertFalse(report.check("armv7", "routing", fail))
            self.assertTrue(report.check("armv7", "shutdown", lambda: None))
            summary = Path(tmp) / "summary.md"
            with patch.dict("os.environ", {"GITHUB_STEP_SUMMARY": str(summary)}):
                report.summary()
            self.assertEqual(json.loads((Path(tmp) / "result.json").read_text())["result"], "FAIL")
            suite = ET.parse(Path(tmp) / "junit.xml").getroot()
            self.assertEqual(suite.attrib["tests"], "2")
            self.assertEqual(suite.attrib["failures"], "1")
            self.assertEqual(suite.find("testcase/failure").text, "wrong backend <llm>")
            self.assertIn("routing | FAIL", summary.read_text())
            self.assertIn("shutdown | PASS", summary.read_text())


class GuestShellTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.adb = Path(directory.name) / "adb.sh"
        patcher = patch.object(run, "ADB", ["sh", str(self.adb)])
        patcher.start()
        self.addCleanup(patcher.stop)

    def simulate_legacy_adb(self, pull='cp "$2" "$3"'):
        # Run the actual wrapper, but lose all shell output and guest exit codes.
        self.adb.write_text(f'''case "$1" in
            shell) sh -c "$2" >/dev/null 2>&1; exit 0 ;;
            pull) {pull} ;;
            *) exit 2 ;;
        esac
        ''')

    def test_lost_shell_output_preserves_result_without_repeating_command(self):
        self.simulate_legacy_adb()
        calls = self.adb.with_name("calls")
        command = f"echo called >>{shlex.quote(str(calls))}; echo output; echo diagnostic >&2"
        self.assertEqual(run.shell(command), "output\ndiagnostic")
        self.assertEqual(calls.read_text(), "called\n")

    def test_pass_message_does_not_hide_nonzero_guest_exit(self):
        self.simulate_legacy_adb()
        with self.assertRaisesRegex(RuntimeError, r"exit 7"):
            run.shell("echo PASS text; exit 7")

    def test_missing_exit_status_or_failed_transfer_never_passes(self):
        for pull, error in [('echo "PASS text" >"$3"', "incomplete guest result"),
                            ('exit 1', "cannot retrieve guest result")]:
            with self.subTest(pull=pull), self.assertRaisesRegex(RuntimeError, error):
                self.simulate_legacy_adb(pull)
                run.shell("echo PASS text")


if __name__ == "__main__":
    unittest.main()
