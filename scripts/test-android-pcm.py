#!/usr/bin/env python3
"""Verify PCM pipe fragmentation, final frames and interruptible input on the host."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]

if __name__ == "__main__":
    compiler = os.environ.get("CC") or shutil.which("clang") or shutil.which("cc")
    if not compiler:
        raise SystemExit("A host C compiler is required")
    with tempfile.TemporaryDirectory(prefix="android-pcm-test-") as directory:
        output = Path(directory) / "pcm-input-test"
        subprocess.run([compiler, "-std=c11", "-O2", "-Wall", "-Wextra", "-Werror",
                        "-o", str(output), "packaging/android/pcm/input_test.c"],
                       cwd=ROOT, check=True)
        subprocess.run([str(output)], check=True, timeout=8)
    print("Android PCM input tests passed")
