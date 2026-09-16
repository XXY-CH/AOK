#!/bin/sh
# SPDX-License-Identifier: GPL-2.0
set -eu
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
build=${AOK_CORE_BUILD:-/tmp/aok-core-build}
output=${AOK_OBJECT_OUTPUT:-"$repo_root/kernel/.build/qemu-arm64-core"}
mkdir -p "$build/initramfs/dev" "$output/arch/arm64/boot"
cp "$output/aok-core-test" "$build/initramfs/init"
(cd "$build/initramfs" && find . -print | cpio -o -H newc) | gzip > "$output/core-initramfs.cpio.gz"
cp "$build/out/arch/arm64/boot/Image" "$output/arch/arm64/boot/Image"
cp "$build/out/.config" "$output/config.enabled"
