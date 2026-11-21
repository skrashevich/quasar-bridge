"""A failed scenario must survive later passes in all CI report formats."""
import contextlib
import io
import json
from pathlib import Path
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

    def test_adb_transport_success_does_not_hide_guest_failure(self):
        for output in (b"all good\n__BRIDGE_RC=1\n", b"no exit marker\n"):
            with self.subTest(output=output), patch.object(run.subprocess, "check_output", return_value=output):
                with self.assertRaises(RuntimeError):
                    run.shell("control -action llm")


if __name__ == "__main__":
    unittest.main()
