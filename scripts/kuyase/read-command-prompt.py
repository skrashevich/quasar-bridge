#!/usr/bin/env python3
"""Hear the utterance accepted by the private native Alice fixture in KUYASE."""
from pathlib import Path
import re
import subprocess
import tempfile


def read_code():
    with tempfile.TemporaryDirectory() as tmp:
        voice = Path(tmp) / "utterance"
        subprocess.run(["adb", "-s", "127.0.0.1:15555", "pull", "/data/bridge-ci/test-speaker/last-utterance", str(voice)],
                       capture_output=True, check=True, timeout=10)
        phrase = voice.read_text()
    match = re.fullmatch(r"Код подтверждения команды — ([1-9][0-9]{2})\. Повторяю, \1\.", phrase)
    if not match:
        raise ValueError("Native Alice did not receive the same three-digit number twice")
    return match[1]


if __name__ == "__main__":
    print(read_code())
