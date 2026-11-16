#!/usr/bin/env python3
"""Build the source-owned OpenSL ES PCM sink for Android ARM64/API 28."""
import argparse
import os
from pathlib import Path
import shutil
import subprocess

ROOT = Path(__file__).resolve().parents[1]


def build(output, ndk=None, cc=None, sysroot=None):
    if cc:
        compiler = shutil.which(str(cc))
        if not compiler or not sysroot:
            raise ValueError("--cc requires an available compiler and --sysroot")
    else:
        directory = (ndk or os.environ.get("ANDROID_NDK_HOME") or
                     os.environ.get("ANDROID_NDK_ROOT") or
                     os.environ.get("ANDROID_NDK_LATEST_HOME"))
        if not directory:
            sdk = os.environ.get("ANDROID_HOME") or os.environ.get("ANDROID_SDK_ROOT")
            installations = sorted((Path(sdk) / "ndk").glob("*"),
                                   key=lambda path: tuple(int(part) if part.isdigit() else 0
                                                          for part in path.name.split("."))) if sdk else []
            if installations:
                directory = installations[-1]
        if not directory:
            raise ValueError("Set ANDROID_NDK_HOME or pass --ndk (or --cc with --sysroot)")
        prebuilt = Path(directory) / "toolchains/llvm/prebuilt"
        candidates = sorted(prebuilt.glob("*/bin/aarch64-linux-android28-clang"))
        if not candidates:
            raise ValueError("NDK does not contain aarch64-linux-android28-clang")
        compiler = str(candidates[0])
    destination = Path(output).resolve()
    destination.parent.mkdir(parents=True, exist_ok=True)
    command = [compiler, "--target=aarch64-linux-android28", "-std=c11", "-O2",
               "-Wall", "-Wextra", "-Werror", "-fPIE", "-fstack-protector-strong",
               "-D_FORTIFY_SOURCE=2", f"-ffile-prefix-map={ROOT}=.",
               "-Wl,--build-id=sha1", "-Wl,-z,relro,-z,now", "-pie", "-pthread"]
    if sysroot:
        command += ["--sysroot=" + str(Path(sysroot).resolve())]
    command += ["packaging/android/pcm/main.c", "-lOpenSLES", "-o", str(destination)]
    subprocess.run(command, cwd=ROOT, check=True)
    destination.chmod(0o755)
    print(destination)
    return destination


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default=ROOT / "dist/android-pcm")
    parser.add_argument("--ndk", help="Android NDK r26d or compatible installation")
    parser.add_argument("--cc", help="explicit Clang driver; requires --sysroot")
    parser.add_argument("--sysroot", help="official Android NDK sysroot")
    arguments = parser.parse_args()
    try:
        build(arguments.output, arguments.ndk, arguments.cc, arguments.sysroot)
    except ValueError as error:
        parser.error(str(error))
