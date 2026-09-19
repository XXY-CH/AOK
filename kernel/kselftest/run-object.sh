#!/bin/sh
# SPDX-License-Identifier: GPL-2.0
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
out_dir=${AOK_OBJECT_OUTPUT:-"$repo_root/kernel/.build/qemu-arm64-object"}
image=${AOK_OBJECT_IMAGE:-"$out_dir/arch/arm64/boot/Image"}
initrd=${AOK_INITRD:-"$out_dir/initramfs.cpio.gz"}
serial=${AOK_SERIAL_LOG:-"$out_dir/serial.log"}
qemu=${QEMU:-qemu-system-aarch64}
test -f "$image"
test -f "$initrd"

# Container-mounted paths can serve stale page cache to QEMU after the
# artifacts are rewritten; boot from a private local copy instead.
boot_dir=$(mktemp -d "${TMPDIR:-/tmp}/aok-boot.XXXXXX")
trap 'kill "${qpid:-}" 2>/dev/null || true; rm -rf "$boot_dir"' EXIT
cp "$image" "$boot_dir/Image"
cp "$initrd" "$boot_dir/initrd"
image="$boot_dir/Image"
initrd="$boot_dir/initrd"

set --
if [ "${AOK_QEMU_NETWORK:-0}" = 1 ]; then
    set -- -netdev user,id=aoknet -device virtio-net-device,netdev=aoknet
fi
"$qemu" -M virt -cpu cortex-a72 -m 512 -smp 2 -nographic -no-reboot "$@" \
    -kernel "$image" -initrd "$initrd" \
    -append "console=ttyAMA0 rdinit=/init panic=-1 ${AOK_TEST_ARGS:-}" > "$serial" 2>&1 &
qpid=$!
trap 'kill "$qpid" 2>/dev/null || true; rm -rf "$boot_dir"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
seconds=0
while kill -0 "$qpid" 2>/dev/null; do
    if [ "$seconds" -ge "${AOK_QEMU_SECONDS:-60}" ]; then
        echo "QEMU timed out: $serial" >&2
        kill "$qpid" 2>/dev/null || true
        wait "$qpid" 2>/dev/null || true
        exit 1
    fi
    sleep 1
    seconds=$((seconds + 1))
done
wait "$qpid"
trap - EXIT INT TERM
cat "$serial"
grep -q "^${AOK_TEST_MARKER:-AOK_OBJECT_TEST}=pass" "$serial"
if grep -Eq '(^not ok |Kernel panic|BUG:|WARNING:|Oops:)' "$serial"; then
    exit 1
fi
