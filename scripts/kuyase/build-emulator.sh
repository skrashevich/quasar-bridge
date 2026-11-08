#!/usr/bin/env bash
# Build on the runner's libc; the upstream Linux binary requires newer glibc.
set -euo pipefail

mkdir -p "$1"
output=$(cd "$1" && pwd)
source_dir="$output/source"
mkdir -p "$source_dir"

# kuyase 1.4.0, pinned to its source commit and archive digest.
curl --fail --location --retry 3 --max-time 180 \
  https://codeload.github.com/vypivshiy/kuyase/tar.gz/0cd9adac36e167fc0dbc934ce9302590a4318856 \
  --output "$output/source.tar.gz"
echo "4207c594f4c34b7c5da6c6f29e2e4d76749713fdefec61ef35e0852fd023412e  $output/source.tar.gz" | sha256sum --check --strict
tar -xzf "$output/source.tar.gz" --strip-components=1 -C "$source_dir"

# Only use the portable archive's ARM guest bridge and kernel, not its host CLI.
curl --fail --location --retry 3 --max-time 180 \
  https://github.com/vypivshiy/kuyase/releases/download/1.4.0/kuyase-linux64-portable.tar.gz \
  --output "$output/portable.tar.gz"
echo "87292ceb4722db3710cbfc5457570bbbdc62b60950e76d64cae98589348800c9  $output/portable.tar.gz" | sha256sum --check --strict
tar -xzf "$output/portable.tar.gz" -C "$output" qemu-hw-bridge kuyase-kernel-arm64.img
mkdir -p "$source_dir/guest-init/bin"
cp "$output/qemu-hw-bridge" "$source_dir/guest-init/bin/qemu-hw-bridge"

cd "$source_dir"
CARGO_TARGET_DIR="$output/target" cargo +stable build --locked --release -p kuyase
cp "$output/target/release/kuyase" "$output/kuyase"
chmod 755 "$output/kuyase" "$output/qemu-hw-bridge"
# Fail here if the resulting host executable cannot load on this runner.
"$output/kuyase" --help
