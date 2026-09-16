#!/bin/sh
set -eu
test "$(uname -s)" = Linux
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
linux=${AOK_SANDBOX_LINUX:-/tmp/aok-p5-build/linux}
work=${AOK_SANDBOX_BUILD:-/tmp/aok-sandbox-build}
out=${AOK_SANDBOX_OUTPUT:-"$source_dir/../../kernel/.build/sandbox"}
mkdir -p "$work/out" "$work/initramfs" "$out"
cp "$source_dir/../../kernel/configs/qemu-arm64-object.fragment" "$work/config"
"$linux/scripts/config" --file "$work/config" --enable SECURITY --enable SECURITY_LANDLOCK --enable SECCOMP --enable SECCOMP_FILTER --enable INET --enable IPV6 --set-str LSM landlock --disable AOK_EXPERIMENTAL
make -C "$linux" O="$work/out" ARCH=arm64 KCONFIG_ALLCONFIG="$work/config" allnoconfig
make -C "$linux" O="$work/out" ARCH=arm64 -j"${AOK_BUILD_JOBS:-6}" Image
for name in aok-sandbox probe qemu-init; do
    cc -std=c11 -Wall -Wextra -Werror -O2 -static "$source_dir/native/$name.c" -o "$work/initramfs/$name"
done
mv "$work/initramfs/qemu-init" "$work/initramfs/init"
(cd "$work/initramfs" && find . -print | cpio -o -H newc) > "$work/initramfs.cpio"
gzip -c "$work/initramfs.cpio" > "$out/initramfs.cpio.gz"
cp "$work/out/arch/arm64/boot/Image" "$out/Image"
cp "$work/out/.config" "$out/config"
sha256sum "$out/Image" "$out/initramfs.cpio.gz" "$out/config" > "$out/sha256.txt"
