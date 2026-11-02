#!/usr/bin/env bash
# Build on the runner's libc; the upstream Linux binary requires newer glibc.
set -euo pipefail

mkdir -p "$1"
output=$(cd "$1" && pwd)
source_dir="$output/source"
mkdir -p "$source_dir"

# kuyase 1.1.0, pinned to its source commit and archive digest.
curl --fail --location --retry 3 --max-time 180 \
  https://codeload.github.com/vypivshiy/kuyase/tar.gz/e83425131e24c7c461330d2a69a14ca7867d6622 \
  --output "$output/source.tar.gz"
echo "597f232470ed6cb9535688e53e139b5b1f049f805e6756c8fca715f4f37b1d59  $output/source.tar.gz" | sha256sum --check --strict
tar -xzf "$output/source.tar.gz" --strip-components=1 -C "$source_dir"

# Only use the portable archive's ARM guest bridge and kernel, not its host CLI.
curl --fail --location --retry 3 --max-time 180 \
  https://github.com/vypivshiy/kuyase/releases/download/1.1.0/kuyase-linux64-portable.tar.gz \
  --output "$output/portable.tar.gz"
echo "a65035617b5fb5bf77e53961f89e0d4751c6662d221467f4c273d82ca0b326bb  $output/portable.tar.gz" | sha256sum --check --strict
tar -xzf "$output/portable.tar.gz" -C "$output" qemu-hw-bridge kuyase-kernel-arm64.img
mkdir -p "$source_dir/guest-init/bin"
cp "$output/qemu-hw-bridge" "$source_dir/guest-init/bin/qemu-hw-bridge"

cd "$source_dir"
CARGO_TARGET_DIR="$output/target" cargo +stable build --locked --release -p kuyase
cp "$output/target/release/kuyase" "$output/kuyase"
chmod 755 "$output/kuyase" "$output/qemu-hw-bridge"
# Fail here if the resulting host executable cannot load on this runner.
"$output/kuyase" --help
