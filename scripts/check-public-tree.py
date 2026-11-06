#!/usr/bin/env python3
"""Scan indexed files without printing credentials or deployment values."""
from pathlib import Path
import ipaddress
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
PRIVATE_NETS = tuple(ipaddress.ip_network((base, mask)) for base, mask in (
    (0x0A000000, 8), (0xAC100000, 12), (0xC0A80000, 16)
))
PATTERNS = {
    "private key": re.compile(rb"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    "GitHub token": re.compile(rb"gh[pousr]_[A-Za-z0-9]{20,}"),
    "personal home path": re.compile(rb"/(?:Users|home)/[A-Za-z0-9_.-]+/"),
    "device serial": re.compile(rb"\bU00[A-Z0-9]{12,29}\b"),
    "device MAC": re.compile(rb"\b(?:[0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}\b"),
}


def findings(data, *, allow_qemu_host=False):
    result = []
    for reason, pattern in PATTERNS.items():
        for match in pattern.finditer(data):
            result.append((data[:match.start()].count(b"\n") + 1, reason))
    for match in re.finditer(rb"(?<![\w.])(?:\d{1,3}\.){3}\d{1,3}(?![\w.])", data):
        try:
            address = ipaddress.ip_address(match.group().decode())
        except ValueError:
            continue
        # QEMU's standard host alias is part of these recorded guest commands,
        # not a deployment address. Keep the exception local to the VHS sources.
        if allow_qemu_host and address == ipaddress.ip_address(0x0A000202):
            continue
        if any(address in network for network in PRIVATE_NETS):
            result.append((data[:match.start()].count(b"\n") + 1, "private network address"))
    return result


def main():
    names = subprocess.check_output(["git", "ls-files", "-z"], cwd=ROOT).split(b"\0")
    failed = False
    for raw in filter(None, names):
        name = raw.decode()
        # Scan the actual index, which is what the next commit will contain.
        data = subprocess.check_output(["git", "show", ":" + name], cwd=ROOT)
        allow_qemu_host = name in ("scripts/vhs/install-api.tape", "scripts/vhs/README.md")
        for line, reason in findings(data, allow_qemu_host=allow_qemu_host):
            print(f"{name}:{line}: {reason}")
            failed = True
    if failed:
        return 1
    print("Indexed tree contains no detected deployment identifiers or secret patterns.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
