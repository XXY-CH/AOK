#!/bin/sh
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
output=${AOK_CORE_OUTPUT:-"$repo/kernel/.build/qemu-arm64-core"}
mkdir -p "$output/probe-initramfs/dev" "$output/probe-initramfs/proc" "$output/probe-initramfs/sys"
(cd "$repo/runtime" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$output/probe-initramfs/init" ./cmd/aok-kernel-probe)
(cd "$output/probe-initramfs" && find . -print | cpio -o -H newc) | gzip > "$output/probe-initramfs.cpio.gz"
AOK_OBJECT_IMAGE=${AOK_OBJECT_IMAGE:-"$output/arch/arm64/boot/Image"} \
AOK_INITRD="$output/probe-initramfs.cpio.gz" \
AOK_SERIAL_LOG="$output/probe-serial.log" AOK_TEST_MARKER=AOK_KERNEL_PROBE \
AOK_TEST_ARGS="${AOK_TEST_ARGS:-}" sh "$repo/kernel/kselftest/run-object.sh"
