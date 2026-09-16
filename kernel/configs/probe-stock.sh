#!/bin/sh
set -eu

# Run on a Linux builder with GNU Make >= 4.0. The source tree is intentionally
# never configured in place; all generated files live in an output directory.

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
linux_dir="$repo_root/kernel/linux"
out_dir=${AOK_KERNEL_OUTPUT:-"$repo_root/kernel/.build/qemu-arm64-stock"}
make_bin=${MAKE:-make}

if [ ! -f "$linux_dir/Makefile" ]; then
    echo "missing kernel/linux; run 'make linux-mount && make linux-fetch'" >&2
    exit 1
fi

case "$(uname -s)" in
    Linux) ;;
    Darwin)
        if [ "${AOK_ALLOW_DARWIN:-}" != 1 ]; then
            echo "stock kernel boot probe must run on Linux; set AOK_ALLOW_DARWIN=1 for config-only preparation" >&2
            exit 2
        fi
        ;;
    *) echo "stock kernel probe must run on Linux; current host is $(uname -s)" >&2; exit 2 ;;
esac

if ! "$make_bin" --version 2>/dev/null | awk 'NR == 1 { if ($3 + 0 < 4.0) exit 1 }'; then
    echo "GNU Make >= 4.0 is required" >&2
    exit 2
fi

mkdir -p "$out_dir"
if [ "$(uname -s)" = Darwin ]; then
    # merge_config.sh uses GNU readlink -m, which is unavailable on macOS.
    # allmodconfig plus KCONFIG_ALLCONFIG applies the fragment without touching the source tree.
    "$make_bin" -C "$linux_dir" O="$out_dir" ARCH=arm64 \
        KCONFIG_ALLCONFIG="$repo_root/kernel/configs/qemu-arm64-aok.fragment" allmodconfig
else
    "$make_bin" -C "$linux_dir" O="$out_dir" ARCH=arm64 defconfig
    "$linux_dir/scripts/kconfig/merge_config.sh" -m -O "$out_dir" \
        "$out_dir/.config" "$repo_root/kernel/configs/qemu-arm64-aok.fragment"
    "$make_bin" -C "$linux_dir" O="$out_dir" ARCH=arm64 olddefconfig
fi

required='CONFIG_BLK_DEV_INITRD CONFIG_DEVTMPFS CONFIG_DEVTMPFS_MOUNT CONFIG_FUSE_FS CONFIG_VIRTIO_FS CONFIG_VSOCKETS CONFIG_VIRTIO_VSOCKETS CONFIG_SECURITY CONFIG_SECURITY_LANDLOCK CONFIG_BPF_SYSCALL CONFIG_BPF_JIT CONFIG_DEBUG_INFO_BTF CONFIG_SCHED_CLASS_EXT CONFIG_KVM'
status=0
for symbol in $required; do
    value=$(sed -n "s/^${symbol}=//p" "$out_dir/.config")
    if [ "$value" != y ] && [ "$value" != m ]; then
        printf 'MISSING %s=%s\n' "$symbol" "${value:-unset}"
        status=1
    else
        printf 'OK      %s=%s\n' "$symbol" "$value"
    fi
done

printf 'config=%s\n' "$out_dir/.config"
exit "$status"
