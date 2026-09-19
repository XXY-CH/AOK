#!/bin/sh
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
output=${AOK_EVENT_PROBE_OUTPUT:-"$repo/kernel/.build/qemu-arm64-p10"}
mkdir -p "$output/event-probe-initramfs/dev" "$output/event-probe-initramfs/proc" "$output/event-probe-initramfs/sys"
(cd "$repo/runtime" && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$output/event-probe-initramfs/init" ./cmd/aok-kernel-event-probe)
(cd "$output/event-probe-initramfs" && find . -print | cpio -o -H newc) | gzip > "$output/event-probe-initramfs.cpio.gz"
AOK_OBJECT_IMAGE=${AOK_OBJECT_IMAGE:-"$output/arch/arm64/boot/Image"} \
AOK_INITRD="$output/event-probe-initramfs.cpio.gz" \
AOK_SERIAL_LOG="$output/event-probe-serial.log" AOK_TEST_MARKER=AOK_EVENT_PROBE \
AOK_QEMU_SECONDS="${AOK_QEMU_SECONDS:-120}" \
AOK_TEST_ARGS="${AOK_TEST_ARGS:-}" sh "$repo/kernel/kselftest/run-object.sh"
