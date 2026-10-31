#!/usr/bin/env python3
"""Build a standalone speaker archive from public files, without YAM tooling."""
import argparse
import hashlib
import io
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PAYLOAD = ("start.sh", "control.sh", "install.sh", "setup.sh", "uninstall.sh",
           "S96quasar-bridge", "firmware.sh", "config.device.example.json")


def build(arch="arm64", output=None, binary=None, tag=None, upx=False):
    if tag is not None and not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", tag):
        raise ValueError("release tag must be vMAJOR.MINOR.PATCH")
    version = tag[1:] if tag else "dev"
    output = Path(output or ROOT / f"dist/quasar-bridge-linux-{arch}.tar.gz").resolve()
    with tempfile.TemporaryDirectory(prefix="quasar-speaker-") as tmp:
        executable = Path(binary).resolve() if binary else Path(tmp) / "quasar-bridge"
        if not binary:
            env = dict(os.environ, CGO_ENABLED="0", GOOS="linux",
                       GOARCH="arm" if arch == "armv7" else "arm64", GOARM="7")
            subprocess.run(["go", "build", "-trimpath", f"-ldflags=-s -w -X quasarbridge/internal/update.Version={version}", "-o",
                            str(executable), "./cmd/quasar-bridge"], cwd=ROOT, env=env, check=True)
        header = executable.read_bytes()[:20]
        elf_class, machine = {"arm64": (2, b"\xb7\x00"), "armv7": (1, b"\x28\x00")}[arch]
        if header[:5] != b"\x7fELF" + bytes([elf_class]) or header[18:20] != machine:
            raise ValueError(f"package requires a Linux {arch} ELF binary")
        if upx:
            if binary:
                packed = Path(tmp) / "quasar-bridge"
                shutil.copy2(executable, packed)
                executable = packed
            # Exhaustive compression search, keeping the fast NRV decompressor.
            subprocess.run(["upx", "--ultra-brute", "--no-lzma", str(executable)], check=True)
            subprocess.run(["upx", "-t", str(executable)], check=True)
        files = [(name, (ROOT / "packaging/speaker" / name).read_bytes()) for name in PAYLOAD]
        files += [(name, (ROOT / name).read_bytes()) for name in ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES", "SOURCE.md")]
        files += [(str(path.relative_to(ROOT)), path.read_bytes())
                  for path in sorted((ROOT / "docs/licenses").glob("*.txt"))]
        files += [("bin/quasar-bridge", executable.read_bytes()),
                  ("ARCH", (arch + "\n").encode()), ("VERSION", (version + "\n").encode())]
        output.parent.mkdir(parents=True, exist_ok=True)
        with tarfile.open(output, "w:gz") as archive:
            for name, data in files:
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = 0o755 if name.endswith(".sh") or name.startswith(("bin/", "S98")) else 0o644
                archive.addfile(info, io.BytesIO(data))
    output.with_suffix(output.suffix + ".sha256").write_text(
        f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n")
    print(output)
    return output


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", choices=("arm64", "armv7"), default="arm64")
    parser.add_argument("--output")
    parser.add_argument("--binary", help="prebuilt static Linux executable for the selected architecture")
    parser.add_argument("--tag", help="stable release tag; otherwise build a dev archive")
    parser.add_argument("--upx", action="store_true",
                        help="compress with UPX --ultra-brute --no-lzma and verify before packaging")
    args = parser.parse_args()
    build(args.arch, args.output, args.binary, args.tag, args.upx)
