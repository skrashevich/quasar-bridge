#!/usr/bin/env python3
"""Bundle corresponding source and vendored Go dependencies for relinking."""
import argparse
import hashlib
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def build(output=None, tag=None):
    if tag is not None and not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", tag):
        raise ValueError("release tag must be vMAJOR.MINOR.PATCH")
    version = tag[1:] if tag else "dev"
    output = Path(output or ROOT / "dist/quasar-bridge-source.tar.gz").resolve()
    with tempfile.TemporaryDirectory(prefix="quasar-source-") as tmp:
        source = Path(tmp) / "quasar-bridge-source"
        source.mkdir()
        names = subprocess.check_output(
            ["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"], cwd=ROOT).split(b"\0")
        for raw in sorted(set(filter(None, names))):
            relative = Path(raw.decode())
            if relative.parts[0] in ("dist", "work", "vendor"):
                continue
            path = ROOT / relative
            if path.is_file() and not path.is_symlink():
                target = source / relative
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(path.read_bytes())
                target.chmod(path.stat().st_mode & 0o777)
        # Vendoring includes the exact linked dependency sources and notices;
        # users can modify go-aac and relink without downloading Go modules.
        subprocess.run(["go", "mod", "vendor", "-o", str(source / "vendor")], cwd=ROOT, check=True)
        (source / "REBUILD.md").write_text(f"""# Rebuilding Quasar Bridge {version}

Use the Go toolchain version recorded in go.mod. All linked Go dependency
sources, including LGPL-3.0 go-aac, are in vendor/. You may modify them and
relink a replacement executable. No module download is needed for the build:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor -trimpath -ldflags '-s -w -X quasarbridge/internal/update.Version={version}' -o quasar-bridge-arm64 ./cmd/quasar-bridge
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -mod=vendor -trimpath -ldflags '-s -w -X quasarbridge/internal/update.Version={version}' -o quasar-bridge-armv7 ./cmd/quasar-bridge
```

Go's own toolchain and system libraries are available from https://go.dev/dl/.
For a modified AAC decoder edit vendor/github.com/skrashevich/go-aac/ before
building. The output replaces app/bin/quasar-bridge in the standalone speaker
installation; stop Bridge before replacement and restart it afterwards.
Pairing state and configuration are outside the executable. No signing key
or authorization token is required to install a modified executable on a
rooted speaker. See docs/guide.md for standalone package installation.

License texts are in LICENSE, THIRD_PARTY_NOTICES and docs/licenses/;
dependency-specific notices are retained in vendor/.
""")
        output.parent.mkdir(parents=True, exist_ok=True)
        with tarfile.open(output, "w:gz") as archive:
            archive.add(source, arcname=source.name)
    output.with_suffix(output.suffix + ".sha256").write_text(
        f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n")
    print(output)
    return output


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output")
    parser.add_argument("--tag")
    args = parser.parse_args()
    build(args.output, args.tag)
