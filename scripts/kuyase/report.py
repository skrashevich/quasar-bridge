"""Persist E2E outcomes in machine-readable and GitHub Actions formats."""
import json
import os
from pathlib import Path
import time
import xml.etree.ElementTree as ET


class Report:
    def __init__(self, board, output):
        self.board = board
        self.output = Path(output)
        self.cases = []

    def check(self, arch, name, function):
        start = time.monotonic()
        case = {"arch": arch, "name": name, "result": "FAIL"}
        try:
            function()
            case["result"] = "PASS"
        except Exception as error:
            case["error"] = str(error)
        case["seconds"] = round(time.monotonic() - start, 3)
        self.cases.append(case)
        self.write()
        print(f'{case["result"]} {self.board}/{arch}/{name}', flush=True)
        if "error" in case:
            print(case["error"], flush=True)
        return case["result"] == "PASS"

    def write(self):
        failed = sum(c["result"] != "PASS" for c in self.cases)
        result = {"board": self.board, "result": "FAIL" if failed or not self.cases else "PASS",
                  "cases": self.cases}
        self.output.mkdir(parents=True, exist_ok=True)
        (self.output / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        suite = ET.Element("testsuite", name=f"kuyase.{self.board}",
                           tests=str(len(self.cases)), failures=str(failed),
                           time=str(round(sum(c["seconds"] for c in self.cases), 3)))
        for case in self.cases:
            test = ET.SubElement(suite, "testcase", classname=f'kuyase.{self.board}.{case["arch"]}',
                                 name=case["name"], time=str(case["seconds"]))
            if case["result"] != "PASS":
                ET.SubElement(test, "failure", message=case["error"]).text = case["error"]
        ET.ElementTree(suite).write(self.output / "junit.xml", encoding="utf-8", xml_declaration=True)

    def summary(self):
        self.write()
        if summary := os.environ.get("GITHUB_STEP_SUMMARY"):
            with open(summary, "a") as stream:
                stream.write(f"\n### КУЯСЭ: {self.board}\n\n| Сборка | Сценарий | Результат | Секунды |\n| --- | --- | --- | --- |\n")
                for case in self.cases:
                    stream.write(f'| {case["arch"]} | {case["name"]} | {case["result"]} | {case["seconds"]} |\n')
