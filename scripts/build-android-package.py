#!/usr/bin/env python3
"""Build an experimental rooted-Android ARM64 archive with manual service scripts."""
import argparse
import hashlib
import io
import os
from pathlib import Path
import shutil
import struct
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PAYLOAD = ("start.sh", "control.sh", "install.sh", "config.device.example.json")


def check_elf(path):
    data = path.read_bytes()
    if (len(data) < 64 or data[:6] != b"\x7fELF\x02\x01"
            or data[18:20] != b"\xb7\x00"):
        raise ValueError("Android archive requires a static ARM64 Linux ELF executable")
    offset = struct.unpack_from("<Q", data, 32)[0]
    entry_size, count = struct.unpack_from("<HH", data, 54)
    if count and (entry_size < 56 or offset + entry_size * count > len(data)):
        raise ValueError("invalid ELF program headers")
    for index in range(count):
        if struct.unpack_from("<I", data, offset + index * entry_size)[0] == 3:
            raise ValueError("Android archive requires a static ELF without an interpreter")


def build(output=None, binary=None):
    output = Path(output or ROOT / "dist/quasar-bridge-android-arm64.tar.gz").resolve()
    with tempfile.TemporaryDirectory(prefix="quasar-android-") as tmp:
        executable = Path(binary).resolve() if binary else Path(tmp) / "quasar-bridge"
        if binary:
            supplied = executable
            executable = Path(tmp) / "quasar-bridge"
            shutil.copy2(supplied, executable)
        if not binary:
            env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="arm64")
            subprocess.run([
                "go", "build", "-trimpath", "-ldflags=-s -w -X quasarbridge/internal/update.Version=dev",
                "-o", str(executable), "./cmd/quasar-bridge",
            ], cwd=ROOT, env=env, check=True)
        check_elf(executable)
        # The common PCM packager owns audio backends and native-library loading.
        tool = os.environ.get("STATION_PCM_PACKAGE_TOOL")
        command = [tool] if tool else ["go", "run", "github.com/skrashevich/station-airplay/cmd/station-pcm-package"]
        command += ["-arch", "arm64", "-binary", str(executable)]
        subprocess.run(command, cwd=ROOT, check=True)
        files = [(name, (ROOT / "packaging/android" / name).read_bytes()) for name in PAYLOAD]
        files += [(name, (ROOT / name).read_bytes())
                  for name in ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES", "SOURCE.md")]
        files += [(str(path.relative_to(ROOT)), path.read_bytes())
                  for path in sorted((ROOT / "docs/licenses").glob("*.txt"))]
        files += [("bin/quasar-bridge", executable.read_bytes()),
                  ("ARCH", b"arm64\n"), ("PLATFORM", b"android\n"), ("VERSION", b"dev\n")]
        output.parent.mkdir(parents=True, exist_ok=True)
        with tarfile.open(output, "w:gz") as archive:
            for name, data in files:
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = 0o755 if name.endswith(".sh") or name.startswith("bin/") else 0o644
                archive.addfile(info, io.BytesIO(data))
    output.with_suffix(output.suffix + ".sha256").write_text(
        f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n")
    print(output)
    return output


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output")
    parser.add_argument("--binary", help="prebuilt static Linux ARM64 dev binary; never modified")
    args = parser.parse_args()
    build(args.output, args.binary)
