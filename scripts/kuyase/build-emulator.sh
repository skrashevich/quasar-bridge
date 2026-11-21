#!/usr/bin/env bash
# Build the headless CLI against the runner's libc, without GUI dependencies.
set -euo pipefail

mkdir -p "$1"
output=$(cd "$1" && pwd)
source_dir="$output/source"
mkdir -p "$source_dir"

# kuyase 1.5.0 headless, pinned to its source commit and archive digest.
curl --fail --location --retry 3 --max-time 180 \
  https://codeload.github.com/vypivshiy/kuyase/tar.gz/c7311d90d6d9168d69fe843367537b3434ea28d4 \
  --output "$output/source.tar.gz"
echo "9e8b96c1f40c3f0e0468ada34570dab37687f8bc1ec50408771b777912b9cc00  $output/source.tar.gz" | sha256sum --check --strict
tar -xzf "$output/source.tar.gz" --strip-components=1 -C "$source_dir"

# Use the headless release's ARM guest bridge and kernel; build the host CLI locally.
curl --fail --location --retry 3 --max-time 180 \
  https://github.com/vypivshiy/kuyase/releases/download/1.5.0/kuyase-headless-linux64.tar.gz \
  --output "$output/guest-assets.tar.gz"
echo "eae7afcca46f12252e96f6a61c6ee8638fb0b7d0ea0995b6082ae0dc44e2ce1f  $output/guest-assets.tar.gz" | sha256sum --check --strict
tar -xzf "$output/guest-assets.tar.gz" -C "$output" qemu-hw-bridge kuyase-kernel-arm64.img
mkdir -p "$source_dir/guest-init/bin"
cp "$output/qemu-hw-bridge" "$source_dir/guest-init/bin/qemu-hw-bridge"

cd "$source_dir"
CARGO_TARGET_DIR="$output/target" cargo +stable build --locked --release --no-default-features -p kuyase
cp "$output/target/release/kuyase" "$output/kuyase"
chmod 755 "$output/kuyase" "$output/qemu-hw-bridge"
# Fail here if the resulting host executable cannot load on this runner.
"$output/kuyase" --help
