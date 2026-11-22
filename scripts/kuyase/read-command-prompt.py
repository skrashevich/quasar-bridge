#!/usr/bin/env python3
"""Recognize the PCM spoken to the emulator's private test speaker."""
from pathlib import Path
import subprocess
import tempfile
import zipfile


def read_code():
    with tempfile.TemporaryDirectory() as tmp:
        recording = Path(tmp) / "prompt.pcm"
        subprocess.run(["adb", "-s", "127.0.0.1:15555", "pull", "/data/bridge-ci/test-speaker/prompt.pcm", str(recording)],
                       capture_output=True, check=True, timeout=10)
        pcm = recording.read_bytes()
    path = Path(__file__).resolve().parents[2] / "internal/commandprompt/russian.bank"
    with zipfile.ZipFile(path) as bank:
        intro = bank.read("intro")
        words = {int(k): bank.read(k) for k in bank.namelist() if k != "intro"}
    if not pcm.startswith(intro):
        raise ValueError("Missing spoken introduction")
    pcm = pcm[len(intro):]
    for candidate in range(100, 1000):
        hundreds, rest = divmod(candidate, 100)
        parts = [hundreds * 100]
        if rest >= 20:
            parts.extend([rest // 10 * 10, rest % 10])
        else:
            parts.append(rest)
        number = b"".join(words[n] for n in parts if n)
        if pcm.startswith(number) and pcm == number + bytes(26460) + number:
            return candidate
    raise ValueError("Speaker did not read the same three-digit number twice")


if __name__ == "__main__":
    print(read_code())
