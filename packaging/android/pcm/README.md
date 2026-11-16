# Android PCM output

This source-owned helper is licensed under Apache-2.0, like Quasar Bridge; see
the package's `LICENSE`. It links Android's system OpenSL ES and Bionic libraries.
It does not include or modify firmware audio binaries.

Build with the official Android NDK r26d (API 28, ARM64):

```sh
ANDROID_NDK_HOME=/absolute/path/android-ndk-r26d \
  python3 scripts/build-android-pcm.py --output dist/android-pcm
```

An existing Clang and official NDK sysroot can also be selected explicitly:

```sh
python3 scripts/build-android-pcm.py --cc /absolute/path/clang \
  --sysroot /absolute/path/ndk/sysroot --output dist/android-pcm
```

The helper accepts the `station-airplay` output arguments:
`-q -D default -t raw -f S16_LE -c 2 -r 44100` (or `48000`). Standard input is
interleaved stereo signed 16-bit little-endian PCM. Volume is applied by Bridge.
Three 20 ms buffers bound audio queueing. The main thread accumulates short pipe
reads; the callback only releases consumed buffers. True EOF drains queued audio;
a partial final stereo frame fails. SIGTERM/SIGINT drops output and releases native
resources. Input polling and queue waits check stop requests every 50 ms; a
stalled queue or EOF drain fails after two seconds.

Run the host input tests without Android audio libraries:

```sh
python3 scripts/test-android-pcm.py
```

These tests verify fragmented input, unpadded final blocks, truncated frames and
interruptible blocked input. Device verification must separately confirm mixer
startup, audible output at both rates, drain and stop while input is blocked.
