Quasar Bridge includes the LGPL-3.0 go-aac decoder through station-airplay.
The corresponding-source archive quasar-bridge-source.tar.gz is distributed
beside each standalone binary release at:
https://github.com/skrashevich/quasar-bridge/releases

It includes Quasar Bridge source, the linked dependencies in vendor/, license
texts and REBUILD.md with commands to modify and relink the executable for
ARM64 or ARMv7. The application code remains Apache-2.0; go-aac retains its
LGPL-3.0 license. A replacement executable can be installed on a rooted speaker
without a signing key. Configuration and pairing state are separate files.

Source archive generation: python3 scripts/build-source-package.py
