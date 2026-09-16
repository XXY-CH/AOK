#!/bin/sh
# SPDX-License-Identifier: GPL-2.0
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
build=${AOK_CORE_BUILD:-/tmp/aok-core-build}
output=${AOK_CORE_OUTPUT:-"$repo/kernel/.build/qemu-arm64-core"}
"$build/linux/scripts/config" --file "$build/out/.config" \
    --enable INET --enable NETDEVICES --enable VIRTIO --enable VIRTIO_MENU \
    --enable VIRTIO_MMIO --enable VIRTIO_NET --enable NET_CORE --enable EVENTFD
make -C "$build/linux" O="$build/out" ARCH=arm64 olddefconfig
make -C "$build/linux" O="$build/out" ARCH=arm64 -j"${AOK_BUILD_JOBS:-4}" Image
cp "$build/out/arch/arm64/boot/Image" "$output/Image.network"
cp "$build/out/.config" "$output/config.network"
