#!/bin/sh
# Boot the full AOK userland stack with the production engine routed through
# the kernel inference device (aok_kernel_infer=1) and drive one complete
# turn over the control plane (aok_boot_probe=turn). Acceptance is the
# AOK_BOOT_TURN=pass marker on the serial console: the turn ran through the
# kernel ainf session with checkpoint and a clean audit chain.
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
runtime="$repo/runtime"
output=${AOK_INITFS_OUTPUT:-"$repo/kernel/.build/qemu-arm64-p10"}
image=${AOK_OBJECT_IMAGE:-"$output/arch/arm64/boot/Image"}
serial=${AOK_INITFS_SERIAL:-"$repo/kernel/.build/aok-initramfs/serial-initfs-turn.log"}
qemu=${QEMU:-qemu-system-aarch64}
seconds=${AOK_INITFS_SECONDS:-120}

test -f "$image"

# Fresh guest binaries: the baked aok-init reads aok_kernel_infer /
# aok_boot_probe from the kernel command line at boot.
mkdir -p "$runtime/.build"
(cd "$runtime" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$runtime/.build/aok-init" ./cmd/aok-init && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$runtime/.build/aok-supervisor" ./cmd/aok-supervisor)
sh "$repo/kernel/initramfs/build-aok.sh"
initrd="$repo/kernel/.build/aok-initramfs/aok-initramfs.cpio.gz"

mkdir -p "$(dirname "$serial")"
: > "$serial"

# Boot from a private local copy: container-mounted paths can serve stale
# page cache to QEMU after the artifacts are rewritten.
boot_dir=$(mktemp -d "${TMPDIR:-/tmp}/aok-turn.XXXXXX")
trap 'kill "${qpid:-}" 2>/dev/null || true; rm -rf "$boot_dir"' EXIT
cp "$image" "$boot_dir/Image"
cp "$initrd" "$boot_dir/initrd"

"$qemu" -M virt -cpu cortex-a72 -m 512 -smp 2 -nographic -no-reboot \
    -kernel "$boot_dir/Image" -initrd "$boot_dir/initrd" \
    -append "console=ttyAMA0 rdinit=/init panic=-1 aok_kernel_infer=1 aok_boot_probe=turn" > "$serial" 2>&1 &
qpid=$!
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for() {
    waited=0
    while [ "$waited" -lt "$seconds" ]; do
        if grep -q "$1" "$serial" 2>/dev/null; then
            return 0
        fi
        if ! kill -0 "$qpid" 2>/dev/null; then
            return 1
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

fail=0
if wait_for 'AOK_BOOT_TURN='; then
    echo "boot-turn: $(grep '^AOK_BOOT_TURN=' "$serial" | tail -1)"
else
    echo "boot-turn: MISSING (see $serial)"
    fail=1
fi
if grep -q '^AOK_KERNEL_INFER=on provider=kernel-infer/' "$serial"; then
    echo "kernel-infer: $(grep '^AOK_KERNEL_INFER=' "$serial" | tail -1)"
else
    echo "kernel-infer: off-or-missing"
    fail=1
fi
if grep -q '^AOK_BOOT_TURN=fail' "$serial"; then
    echo "boot-turn reported failure"
    fail=1
fi
ready_count=$(grep -c 'AOK_SUPERVISOR_READY=' "$serial" || true)
echo "ready-markers: $ready_count"
if [ "$ready_count" -ne 1 ]; then
    echo "unexpected supervisor restart count"
    fail=1
fi
if grep -Eq 'Kernel panic|Attempted to kill init|BUG:|WARNING:|Oops:' "$serial"; then
    echo "guest fault detected (see $serial)"
    fail=1
fi

kill "$qpid" 2>/dev/null || true
wait "$qpid" 2>/dev/null || true
trap - INT TERM
if [ "$fail" -ne 0 ]; then
    tail -30 "$serial"
    exit 1
fi
echo "AOK_INITFS_TURN_BOOT=pass"
