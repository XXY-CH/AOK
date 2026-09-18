#!/bin/sh
# SPDX-License-Identifier: GPL-2.0
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
build_dir=${AOK_OBJECT_BUILD:-/tmp/aok-object-build}
out_dir=${AOK_OBJECT_OUTPUT:-"$repo_root/kernel/.build/qemu-arm64-object"}
baseline=f6388029ea9e2c9e807d73827658738ea131faee
source_dir="$build_dir/linux"
object_dir="$build_dir/out"

test "$(uname -s)" = Linux || { echo 'Run this script in a Linux arm64 builder.' >&2; exit 1; }
test "$(uname -m)" = aarch64 || { echo 'A native arm64 toolchain is required.' >&2; exit 1; }
test "$(git -C "$repo_root/kernel/linux" rev-parse HEAD)" = "$baseline"
test ! -e "$source_dir" || { echo "Use a fresh AOK_OBJECT_BUILD: $source_dir exists" >&2; exit 1; }
mkdir -p "$source_dir" "$object_dir" "$out_dir"
# An archive makes the patched build independent of baseline working-tree edits.
git -C "$repo_root/kernel/linux" archive "$baseline" > "$build_dir/baseline.tar"
tar -xf "$build_dir/baseline.tar" -C "$source_dir"
rm "$build_dir/baseline.tar"
patch_list=${AOK_PATCH_LIST:-"$repo_root/kernel/patches/series"}
while IFS= read -r patch_name || [ -n "$patch_name" ]; do
    case "$patch_name" in ''|'#'*) continue ;; esac
    git -C "$source_dir" apply --check "$repo_root/kernel/patches/$patch_name"
    git -C "$source_dir" apply "$repo_root/kernel/patches/$patch_name"
done < "$patch_list"

make -C "$source_dir" O="$object_dir" ARCH=arm64 \
    KCONFIG_ALLCONFIG="$repo_root/kernel/configs/qemu-arm64-object.fragment" allnoconfig
grep -qx 'CONFIG_AOK_EXPERIMENTAL=y' "$object_dir/.config"
make -C "$source_dir" O="$object_dir" ARCH=arm64 -j"${AOK_BUILD_JOBS:-4}" Image
make -C "$source_dir" O="$object_dir" ARCH=arm64 headers_install INSTALL_HDR_PATH="$out_dir/headers"
make -C "$repo_root/kernel/kselftest" LINUX="$source_dir" OUTPUT="$out_dir"
mkdir -p "$out_dir/arch/arm64/boot" "$build_dir/initramfs/dev" "$build_dir/initramfs/proc"
cp "$object_dir/arch/arm64/boot/Image" "$out_dir/arch/arm64/boot/Image"
cp "$object_dir/.config" "$out_dir/config.enabled"
pack_initramfs() {
    cp "$out_dir/$1" "$build_dir/initramfs/init"
    (cd "$build_dir/initramfs" && find . -print | cpio -o -H newc > "$build_dir/initramfs.cpio")
    gzip -c "$build_dir/initramfs.cpio" > "$out_dir/$2"
}
pack_initramfs aok-object-test initramfs.cpio.gz
pack_initramfs aok-task-test task-initramfs.cpio.gz
pack_initramfs aok-resource-test resource-initramfs.cpio.gz
pack_initramfs aok-eventsrc-test eventsrc-initramfs.cpio.gz
pack_initramfs aok-eventwake-test eventwake-initramfs.cpio.gz
pack_initramfs aok-core-test core-initramfs.cpio.gz
pack_initramfs aok-core-test core-initramfs.cpio.gz
"$source_dir/scripts/config" --file "$object_dir/.config" --disable AOK_EXPERIMENTAL
make -C "$source_dir" O="$object_dir" ARCH=arm64 olddefconfig
make -C "$source_dir" O="$object_dir" ARCH=arm64 -j"${AOK_BUILD_JOBS:-4}" Image
cp "$object_dir/arch/arm64/boot/Image" "$out_dir/Image.disabled"
cp "$object_dir/.config" "$out_dir/config.disabled"
{
    printf 'BASELINE=%s\n' "$baseline"
    gcc --version | head -1
    make --version | head -1
    cat /etc/os-release
    sha256sum "$out_dir/arch/arm64/boot/Image" "$out_dir/Image.disabled" \
        "$out_dir/initramfs.cpio.gz" "$out_dir/config.enabled" "$out_dir/config.disabled"
    while IFS= read -r patch_name || [ -n "$patch_name" ]; do
        case "$patch_name" in ''|'#'*) continue ;; esac
        sha256sum "$repo_root/kernel/patches/$patch_name"
    done < "$patch_list"
} > "$out_dir/build-manifest.txt"
printf 'BASELINE=%s\n' "$baseline"
printf 'BUILDER=%s\n' "$(uname -m) $(gcc -dumpfullversion)"
printf 'IMAGE=%s\nINITRD=%s\n' "$out_dir/arch/arm64/boot/Image" "$out_dir/initramfs.cpio.gz"
