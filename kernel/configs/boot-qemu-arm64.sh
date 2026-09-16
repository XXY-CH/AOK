#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
out_dir=${AOK_KERNEL_OUTPUT:-"$repo_root/kernel/.build/qemu-arm64-stock"}
image="$out_dir/arch/arm64/boot/Image"
initrd=${AOK_INITRD:-"$repo_root/kernel/.build/qemu-initramfs/qemu-initramfs.cpio.gz"}
serial=${AOK_SERIAL_LOG:-"$out_dir/serial.log"}
qemu=${QEMU:-qemu-system-aarch64}

test -f "$image" || { printf 'missing kernel Image: %s\n' "$image" >&2; exit 1; }
test -f "$initrd" || { printf 'missing initramfs: %s\n' "$initrd" >&2; exit 1; }

: > "$serial"
"$qemu" \
    -M virt -cpu cortex-a72 -m "${AOK_QEMU_MEMORY:-1024}" -smp "${AOK_QEMU_SMP:-2}" \
    -nographic -kernel "$image" -initrd "$initrd" \
    -append 'console=ttyAMA0 rdinit=/init lsm=landlock,capability' \
    > "$serial" 2>&1 &
qpid=$!
trap 'kill "$qpid" 2>/dev/null || true' EXIT INT TERM

sleep "${AOK_QEMU_SECONDS:-10}"
kill "$qpid" 2>/dev/null || true
wait "$qpid" 2>/dev/null || true
trap - EXIT INT TERM

printf 'QEMU_SERIAL=%s\n' "$serial"
rg -n 'AOK_PROBE_|Kernel panic|No working init' "$serial" || true
