"""Pinned firmware inputs and CI matrix for the KUYASE station emulator."""
import argparse
from dataclasses import dataclass
import json
import os
from pathlib import Path


DATASET_REV = "8a027abcba7920a570192cf38c9689f344de66f0"
DATASET = "https://huggingface.co/datasets/vypivshiy/iot-quasar-dataset"
ARCHES = ("arm64", "armv7")


@dataclass(frozen=True)
class Firmware:
    url: str
    ota_sha256: str
    member_sha256: str
    member: str = "rootfs.ubifs"
    wrapper: str = "cpio"
    dataset_path: str = ""
    adbd_backup: bool = False


def dataset_ota(path, ota_sha256, member_sha256, *, member="rootfs.ubifs"):
    return Firmware(f"{DATASET}/resolve/{DATASET_REV}/{path}", ota_sha256,
                    member_sha256, member=member, dataset_path=path)


# Preserve the tested OTA pins for the mandatory checks.
CORE = {
    "yandexmini": Firmware(
        "https://quasar.s3.yandex.net/yandexmini/ota/release/"
        "d165ea71-76cd-4df5-84f8-0441526d1124/1.373.4.13.4819025044.20261030.zip",
        "80ca52832f78403335faa45e050c5828d63facdd89dba9f456de5d82fd20c98f",
        "c057fbafcd960f0f0329400040cf974a749b74f8261be2e858d164ee7087c1c4",
        wrapper="xz", adbd_backup=True,
    ),
    "station2": Firmware(
        "https://quasar.s3.yandex.net/yandexmidi/ota/user/"
        "949c532f-f0fe-4363-95c8-5479c410dfd1/0.373.1.13.4819000379.20261030.299.zip",
        "22e6783c88b6947c8a7a2243e9511dede07f12df083edae89b95a2ee65f0fc2e",
        "ec7857bd96eed02785596e31b795b2a2154343e1cfbbb2124b157cd42bec4aee",
    ),
}

# Curated intersection, one selected OTA per distinct emulator profile.
# OTA checksums come from catalog.json; rootfs member hashes were computed
# from those verified bytes. No board is inferred from an OTA filename.
EXTENDED = {
    "yandexmicro": dataset_ota(
        "yandexmicro/0.369.1.48.4790768027.20261019.297/0.369.1.48.4790768027.20261019.297.zip",
        "2dab769d5301b2ce459ef254a29a104e71860ef892d57f181a29733d25b491f9",
        "b5d4b191682bc7693600f602a4de3a3df5d3b9ee5d2e5bc9a4510730ec7afac3",
    ),
    "yandexmini2nc": dataset_ota(
        "yandexmini_2/0.369.1.46.4793643989.20261020.299/0.369.1.46.4793643989.20261020.299.zip",
        "e1993924a438edb20cce370c6be3089a9426d8f3377874197301d450ef0ad854",
        "3ea88dc526b867b64a10ea6ad5c8c894e117581fa0350ec3449e22e0ab367905",
    ),
    "cucumber": dataset_ota(
        "cucumber/0.369.1.46.4778262955.20261013.160/0.369.1.46.4778262955.20261013.160.zip",
        "5b7858a7e18130d5ab962566713cbe30ce63f3d09973983fee2752fd960f7dbb",
        "a00231e113cb376cc0515d85b00310a5d2116f4b8977c19d3aedf8c960e06426",
        member="rootfs.ext4.gz",
    ),
}
FIRMWARE = CORE | EXTENDED

# All remaining dataset platforms at DATASET_REV, without a confirmed profile
# in KUYASE 1.5.0. This is independent of the Bridge's real device support.
UNSUPPORTED = (
    "elari_a98", "jbl_link_music", "jbl_link_portable", "lightcomm",
    "linkplay_a98", "prestigio_smart_mate", "yandexstation",
)


def matrix(event, ref):
    extended = event in ("pull_request", "workflow_dispatch") or (event == "push" and ref == "refs/heads/main")
    selected = FIRMWARE if extended else CORE
    return {"include": [{"board": board, "arch": arch}
                        for board in selected for arch in ARCHES]}


def summary(rows):
    selected = {row["board"] for row in rows["include"]}
    text = ["### KUYASE: запланированные проверки", "",
            "| Профиль | Источник | Покрытие |", "| --- | --- | --- |"]
    for board, fw in FIRMWARE.items():
        source = ("закреплённая OTA Яндекса" if board in CORE else
                  f"[`{fw.dataset_path.split('/')[1]}`]({fw.url})")
        coverage = "ARM64, ARMv7" if board in selected else "не запускается на этом событии"
        text.append(f"| `{board}` | {source} | {coverage} |")
    text += ["", f"[Версия каталога]({DATASET}/commit/{DATASET_REV}).",
             "Mini 1 и Станция 2 используют прежние OTA, не альтернативные сборки датасета.",
             "Без подтверждённого профиля КУЯСЭ: " + ", ".join(f"`{p}`" for p in UNSUPPORTED) + ".",
             "Mini 2 с часами, Лайт 2 и другие отсутствующие в датасете варианты не проверяются.", ""]
    return "\n".join(text)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--event", required=True)
    parser.add_argument("--ref", required=True)
    args = parser.parse_args()
    rows = matrix(args.event, args.ref)
    rendered = json.dumps(rows, separators=(",", ":"))
    if out := os.environ.get("GITHUB_OUTPUT"):
        with Path(out).open("a") as target:
            print(f"matrix={rendered}", file=target)
    else:
        print(rendered)
    if out := os.environ.get("GITHUB_STEP_SUMMARY"):
        with Path(out).open("a") as target:
            target.write(summary(rows))


if __name__ == "__main__":
    main()
