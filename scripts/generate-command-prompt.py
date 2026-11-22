#!/usr/bin/env python3
"""Regenerate the offline Russian prompt with espeakng-loader==0.2.4 (eSpeak NG 1.52)."""
import array
import ctypes as C
from pathlib import Path
import sys
import zipfile

import espeakng_loader

WORDS = {
    "intro": "Подтверждение команды. Ваш код.",
    **dict(enumerate("один два три четыре пять шесть семь восемь девять десять одиннадцать двенадцать тринадцать четырнадцать пятнадцать шестнадцать семнадцать восемнадцать девятнадцать".split(), 1)),
    **dict(zip(range(20, 100, 10), "двадцать тридцать сорок пятьдесят шестьдесят семьдесят восемьдесят девяносто".split())),
    **dict(zip(range(100, 1000, 100), "сто двести триста четыреста пятьсот шестьсот семьсот восемьсот девятьсот".split())),
}


def main():
    lib = C.CDLL(espeakng_loader.get_library_path())
    data = Path(espeakng_loader.get_data_path()).parent
    if lib.espeak_Initialize(2, 0, bytes(data), 0) != 22050 or lib.espeak_SetVoiceByName(b"ru"):
        raise RuntimeError("Russian voice at 22050 Hz required")
    chunks = []

    @C.CFUNCTYPE(C.c_int, C.POINTER(C.c_short), C.c_int, C.c_void_p)
    def receive(wav, count, _events):
        if count:
            chunks.append(C.string_at(wav, count * 2))
        return 0

    lib.espeak_SetSynthCallback(receive)
    lib.espeak_SetParameter(1, 145, 0)
    target = Path(__file__).resolve().parents[1] / "internal/commandprompt/russian.bank"
    with zipfile.ZipFile(target, "w") as bank:
        for key, word in WORDS.items():
            chunks.clear()
            text = word.encode() + b"\0"
            if lib.espeak_Synth(text, C.c_size_t(len(text)), 0, 1, 0, 1, None, None):
                raise RuntimeError("synthesis failed")
            samples = array.array("h", b"".join(chunks))
            # Keep a short natural pause between words, cutting long end-of-sentence silence.
            end = next(i for i in range(len(samples) - 1, 0, -1) if abs(samples[i]) > 80)
            samples = samples[:end + 1]
            samples.extend([0] * 1800)
            if sys.byteorder == "big":
                samples.byteswap()
            entry = zipfile.ZipInfo(str(key))
            bank.writestr(entry, samples.tobytes(), compress_type=zipfile.ZIP_DEFLATED)
    lib.espeak_Terminate()


if __name__ == "__main__":
    main()
