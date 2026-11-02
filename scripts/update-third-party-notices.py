#!/usr/bin/env python3
"""Collect license notices for all executables in the Linux speaker package."""
from pathlib import Path
import os
import subprocess

ROOT = Path(__file__).resolve().parents[1]


def main():
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="arm64")
    output = subprocess.check_output(
        ["go", "list", "-deps", "-f", "{{if .Module}}{{.Module.Path}}|{{.Module.Version}}|{{.Module.Dir}}{{end}}", "./cmd/quasar-bridge", "github.com/skrashevich/station-airplay/cmd/station-pcm", "github.com/skrashevich/station-airplay/cmd/station-pcm-package"],
        cwd=ROOT, env=env, text=True,
    )
    modules = {}
    for line in output.splitlines():
        if line and not line.startswith("quasarbridge|"):
            name, version, directory = line.split("|", 2)
            modules[name + " " + version] = Path(directory)
    goroot = subprocess.check_output(["go", "env", "GOROOT"], cwd=ROOT, env=env, text=True).strip()
    groups = {}
    for name, directory in sorted(modules.items()):
        notices = []
        for filename in ("LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING", "COPYRIGHT", "NOTICE"):
            path = directory / filename
            if path.is_file():
                notices.append(path.read_text().strip())
        if notices:
            groups.setdefault("\n\n".join(notices), []).append(name)
        else:
            # Generated protocol bindings may not ship a separate license file.
            # Preserve their identity without assigning this project's license.
            groups.setdefault("No separate license notice is present in the module archive. Refer to the module's upstream source.", []).append(name)
    groups.setdefault((Path(goroot) / "LICENSE").read_text().strip(), []).append("Go standard library and runtime")
    parts = ["Third-party components linked into Quasar Bridge\n\nThese components retain their own licenses. This file does not relicense them.\n"]
    for notice, names in sorted(groups.items(), key=lambda item: item[1][0]):
        parts.append("\n" + "\n".join(names) + "\n\n" + notice + "\n")
    (ROOT / "THIRD_PARTY_NOTICES").write_text("\n".join(line.rstrip() for line in "\n".join(parts).splitlines()) + "\n")
    print(f"Updated third-party notices for {len(modules)} modules and the Go runtime.")


if __name__ == "__main__":
    main()
