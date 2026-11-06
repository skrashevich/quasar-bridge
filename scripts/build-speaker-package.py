#!/usr/bin/env python3
"""Build a standalone speaker archive from public files, without YAM tooling."""
import argparse
import hashlib
import io
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
PAYLOAD = ("start.sh", "control.sh", "install.sh", "setup.sh", "uninstall.sh",
           "S96quasar-bridge", "firmware.sh", "config.device.example.json")


def check_elf(path, arch):
    header = path.read_bytes()[:20]
    elf_class, machine = {"arm64": (2, b"\xb7\x00"), "armv7": (1, b"\x28\x00")}[arch]
    if header[:6] != b"\x7fELF" + bytes([elf_class, 1]) or header[18:20] != machine:
        raise ValueError(f"package requires a Linux {arch} ELF binary: {path.name}")


def build(arch="arm64", output=None, binary=None, tag=None, upx=False,
          pcm_armv7_binary=None, pcm_arm64_binary=None):
    if tag is not None and not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", tag):
        raise ValueError("release tag must be vMAJOR.MINOR.PATCH")
    version = tag[1:] if tag else "dev"
    output = Path(output or ROOT / f"dist/quasar-bridge-linux-{arch}.tar.gz").resolve()
    with tempfile.TemporaryDirectory(prefix="quasar-speaker-") as tmp:
        executable = Path(binary).resolve() if binary else Path(tmp) / "quasar-bridge"
        if binary:
            # Bundling and optional compression must never modify a supplied
            # executable. The installed artifact is still just this one file.
            supplied = executable
            executable = Path(tmp) / "quasar-bridge"
            shutil.copy2(supplied, executable)
        if not binary:
            env = dict(os.environ, CGO_ENABLED="0", GOOS="linux",
                       GOARCH="arm" if arch == "armv7" else "arm64", GOARM="7")
            subprocess.run(["go", "build", "-trimpath", f"-ldflags=-s -w -X quasarbridge/internal/update.Version={version}", "-o",
                            str(executable), "./cmd/quasar-bridge"], cwd=ROOT, env=env, check=True)
        check_elf(executable, arch)
        # station-airplay embeds both required audio ABIs inside the receiver.
        # Only this completed ELF is installed; no companion files are needed.
        tool = os.environ.get("STATION_PCM_PACKAGE_TOOL")
        command = [tool] if tool else ["go", "run", "github.com/skrashevich/station-airplay/cmd/station-pcm-package"]
        command += ["-arch", arch, "-binary", str(executable)]
        env = dict(os.environ)
        if upx:
            # The producer strips any earlier payload, compresses and verifies
            # the ELF, then appends the new payload as the final build step.
            command += ["-upx"]
            real_upx = shutil.which("upx")
            if not real_upx:
                raise FileNotFoundError("UPX executable is required for --upx")
            # The producer currently hardcodes --ultra-brute. Adapt only that
            # argument, retaining its payload handling and integrity check.
            shim_dir = Path(tmp) / "upx-bin"
            shim_dir.mkdir()
            shim = shim_dir / "upx"
            shim.write_text(
                '#!/bin/sh\n'
                'if [ "$1" = --ultra-brute ]; then\n'
                '    shift\n'
                '    set -- -6 "$@"\n'
                'fi\n'
                f'exec {shlex.quote(str(Path(real_upx).resolve()))} "$@"\n'
            )
            shim.chmod(0o755)
            env["PATH"] = str(shim_dir) + os.pathsep + env.get("PATH", "")
        for helper_arch, supplied in (("armv7", pcm_armv7_binary), ("arm64", pcm_arm64_binary)):
            if supplied:
                command += [f"-{helper_arch}-binary", str(Path(supplied).resolve())]
        subprocess.run(command, cwd=ROOT, env=env, check=True)
        files = [(name, (ROOT / "packaging/speaker" / name).read_bytes()) for name in PAYLOAD]
        files += [(name, (ROOT / name).read_bytes()) for name in ("LICENSE", "NOTICE", "THIRD_PARTY_NOTICES", "SOURCE.md")]
        files += [("docs/soundcloud.md", (ROOT / "docs/soundcloud.md").read_bytes())]
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
    parser.add_argument("--pcm-armv7-binary", help="prebuilt ARMv7 PCM payload to embed; otherwise build it")
    parser.add_argument("--pcm-arm64-binary", help="prebuilt ARM64 PCM payload to embed; otherwise build it")
    parser.add_argument("--tag", help="stable release tag; otherwise build a dev archive")
    parser.add_argument("--upx", action="store_true",
                        help="compress with UPX -6 --no-lzma and verify before packaging")
    args = parser.parse_args()
    build(args.arch, args.output, args.binary, args.tag, args.upx,
          args.pcm_armv7_binary, args.pcm_arm64_binary)
