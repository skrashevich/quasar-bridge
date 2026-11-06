Quasar Bridge includes the LGPL-3.0 go-aac decoder through station-airplay.
The corresponding-source archive quasar-bridge-source.tar.gz is distributed
beside each standalone binary release at:
https://github.com/skrashevich/quasar-bridge/releases

It includes Quasar Bridge source, the linked dependencies in vendor/, license
texts and REBUILD.md with commands to modify and relink the executable for
ARM64 or ARMv7. The application code remains Apache-2.0; go-aac retains its
LGPL-3.0 license. A replacement executable can be installed on a rooted speaker
without a signing key. Configuration and pairing state are separate files.

The source archive also vendors station-airplay, including its PCM implementation,
single-file bundler and purego dependency. REBUILD.md describes how to embed the
PCM modules in the final Bridge executable. Only that one executable is installed;
its selected PCM module is materialized in a private cache on demand. The speaker's
existing ALSA library is not copied into the binary package.

Source archive generation: python3 scripts/build-source-package.py

Realtime ALAC decoding uses the pure-Go saprobe-alac module, licensed under
Apache-2.0, with its Apple source attribution retained in THIRD_PARTY_NOTICES.
Its exact source is included in vendor/ in the corresponding-source archive.
No CGO or native codec library is required for either audio decoder.
