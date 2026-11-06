#!/usr/bin/env python3
"""Build kuyase images from pinned public OTAs; never execute their updaters."""
import argparse
import hashlib
import io
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile


FIRMWARE = {
    "yandexmini": (
        "https://quasar.s3.yandex.net/yandexmini/ota/release/"
        "d165ea71-76cd-4df5-84f8-0441526d1124/1.373.4.13.4819025044.20261030.zip",
        "80ca52832f78403335faa45e050c5828d63facdd89dba9f456de5d82fd20c98f",
        "c057fbafcd960f0f0329400040cf974a749b74f8261be2e858d164ee7087c1c4",
    ),
    "station2": (
        "https://quasar.s3.yandex.net/yandexmidi/ota/user/"
        "949c532f-f0fe-4363-95c8-5479c410dfd1/0.373.1.13.4819000379.20261030.299.zip",
        "22e6783c88b6947c8a7a2243e9511dede07f12df083edae89b95a2ee65f0fc2e",
        "ec7857bd96eed02785596e31b795b2a2154343e1cfbbb2124b157cd42bec4aee",
    ),
}


def checked(data, digest):
    if hashlib.sha256(data).hexdigest() != digest:
        raise ValueError("firmware SHA-256 mismatch")
    return data


def cpio_member(data, wanted):
    """Read a SWUpdate newc/CRC archive without extracting filesystem paths."""
    offset = 0
    while offset + 110 <= len(data):
        header = data[offset:offset + 110]
        if header[:6] not in (b"070701", b"070702"):
            raise ValueError("expected SWUpdate CPIO header")
        size = int(header[54:62], 16)
        name_size = int(header[94:102], 16)
        name = data[offset + 110:offset + 110 + name_size].rstrip(b"\0")
        start = (offset + 110 + name_size + 3) & ~3
        end = start + size
        if not name_size or end > len(data):
            raise ValueError("truncated SWUpdate CPIO member")
        if name.decode() == wanted:
            return data[start:end]
        if name == b"TRAILER!!!":
            break
        offset = (end + 3) & ~3
    raise ValueError(f"missing OTA member: {wanted}")


def rootfs_tar(image, directory, board):
    from ubireader.scripts import ubireader_extract_files
    from ubireader.ubifs import output

    metadata = {}

    def retain_inode(path, inode):
        info = inode["ino"]
        metadata[str(Path(path).relative_to(directory))] = (
            info.mode & 0o7777, info.uid, info.gid)

    # Preserve original inode modes/owners in the tar, without root/chown.
    output._set_file_perms = retain_inode
    sys.argv = ["ubireader_extract_files", "-k", "-o", str(directory), str(image)]
    ubireader_extract_files.main()
    if board == "yandexmini":
        # Mini 1 stores the stock daemon under this backup name.
        source = directory / "usr/bin/adbd_backup"
        shutil.copyfile(source, directory / "usr/bin/adbd")
        metadata["usr/bin/adbd"] = (0o755, 0, 0)

    def guest_metadata(info):
        info.mode, info.uid, info.gid = metadata.get(info.name, (info.mode, 0, 0))
        info.uname = info.gname = ""
        info.pax_headers = {}
        return info

    archive = directory.with_suffix(".tar")
    with tarfile.open(archive, "w", format=tarfile.PAX_FORMAT) as target:
        for path in sorted(directory.iterdir()):
            target.add(path, arcname=path.name, filter=guest_metadata)
    return archive


def prepare_image(emulator, board, archive, log_path):
    # Always mirror the child output to CI, including loader/GLIBC failures.
    try:
        with log_path.open("w") as log:
            subprocess.run([str(emulator / "kuyase"), "prepare", "--board", board,
                            "--dump", str(archive)], cwd=emulator, stdout=log,
                           stderr=subprocess.STDOUT, check=True, timeout=180)
    finally:
        print(log_path.read_text(errors="replace"), end="", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--board", choices=FIRMWARE, required=True)
    parser.add_argument("--emulator-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    out = args.output.resolve()
    out.mkdir(parents=True, exist_ok=True)
    url, ota_hash, rootfs_hash = FIRMWARE[args.board]
    ota = out / "ota.bin"
    subprocess.run(["curl", "--fail", "--location", "--retry", "3", "--max-time",
                    "180", url, "--output", str(ota)], check=True)
    data = checked(ota.read_bytes(), ota_hash)
    if args.board == "yandexmini":
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:xz") as archive:
            data = archive.extractfile("./yandex_io_ota_enc.swu").read()
    image = out / "rootfs.ubifs"
    image.write_bytes(checked(cpio_member(data, "rootfs.ubifs"), rootfs_hash))
    archive = rootfs_tar(image, out / "rootfs", args.board)
    emulator = args.emulator_dir.resolve()
    # Ensures the portable CLI resolves this directory as its workspace.
    (emulator / "work").mkdir(exist_ok=True)
    prepare_image(emulator, args.board, archive, out / "prepare.log")
    expected = emulator / "work" / f"{args.board}-system.ext4"
    if not expected.is_file():
        raise RuntimeError(f"kuyase did not produce {expected.name}")


if __name__ == "__main__":
    main()
