#!/bin/sh
# Boot the full AOK userland stack (aok-init PID1 -> aok-supervisor) on the
# AOK kernel in QEMU and verify the readiness markers on the serial console.
# The guest has no shutdown path yet (vsock control plane is a later slice),
# so the harness polls the serial log and then stops QEMU.
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
output=${AOK_INITFS_OUTPUT:-"$repo/kernel/.build/qemu-arm64-p10"}
image=${AOK_OBJECT_IMAGE:-"$output/arch/arm64/boot/Image"}
initrd=${AOK_INITFS:-"$repo/kernel/.build/aok-initramfs/aok-initramfs.cpio.gz"}
serial=${AOK_INITFS_SERIAL:-"$repo/kernel/.build/aok-initramfs/serial-initfs-boot.log"}
qemu=${QEMU:-qemu-system-aarch64}
seconds=${AOK_INITFS_SECONDS:-90}
hold=${AOK_INITFS_HOLD:-6}

test -f "$image"
test -f "$initrd"
mkdir -p "$(dirname "$serial")"
: > "$serial"

# Boot from a private local copy: container-mounted paths can serve stale
# page cache to QEMU after the artifacts are rewritten.
boot_dir=$(mktemp -d "${TMPDIR:-/tmp}/aok-initfs.XXXXXX")
trap 'kill "${qpid:-}" 2>/dev/null || true; rm -rf "$boot_dir"' EXIT
cp "$image" "$boot_dir/Image"
cp "$initrd" "$boot_dir/initrd"
image="$boot_dir/Image"
initrd="$boot_dir/initrd"

"$qemu" -M virt -cpu cortex-a72 -m 512 -smp 2 -nographic -no-reboot \
    -kernel "$image" -initrd "$initrd" \
    -append "console=ttyAMA0 rdinit=/init panic=-1" > "$serial" 2>&1 &
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
if wait_for 'AOK_SUPERVISOR_READY='; then
    echo "supervisor-ready: yes"
else
    echo "supervisor-ready: MISSING (see $serial)"
    fail=1
fi
if grep -q '^AOK_KERNEL_BRIDGE=on' "$serial"; then
    echo "kernel-bridge: on"
else
    echo "kernel-bridge: off-or-missing"
    fail=1
fi

# Stability window: the stack must stay up instead of crash-looping.
slept=0
while [ "$slept" -lt "$hold" ]; do
    if ! kill -0 "$qpid" 2>/dev/null; then
        break
    fi
    sleep 1
    slept=$((slept + 1))
done
ready_count=$(grep -c 'AOK_SUPERVISOR_READY=' "$serial" || true)
echo "ready-markers: $ready_count hold: ${slept}s"
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
    tail -20 "$serial"
    exit 1
fi
echo "AOK_INITFS_BOOT=pass"
