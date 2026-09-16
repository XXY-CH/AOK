#!/bin/sh

set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
out_dir=${1:-"$root_dir/kernel/.build/qemu-initramfs"}
work_dir="$out_dir/root"
archive="$out_dir/qemu-initramfs.cpio.gz"

command -v busybox >/dev/null 2>&1 || {
    printf '%s\n' 'busybox is required (use busybox-static on Debian).' >&2
    exit 1
}
command -v cpio >/dev/null 2>&1 || {
    printf '%s\n' 'cpio is required.' >&2
    exit 1
}

rm -rf "$work_dir"
mkdir -p "$work_dir/bin" "$work_dir/dev" "$work_dir/proc" "$work_dir/sys" "$work_dir/tmp"
cp "$root_dir/kernel/initramfs/qemu-init" "$work_dir/init"
chmod 0755 "$work_dir/init"
cp "$(command -v busybox)" "$work_dir/bin/busybox"
for applet in sh mount cat uname printf ls sleep; do
    ln -s busybox "$work_dir/bin/$applet"
done

(cd "$work_dir" && find . -print | cpio -o -H newc 2>/dev/null | gzip -9 > "$archive")
printf 'INITRAMFS=%s\n' "$archive"
