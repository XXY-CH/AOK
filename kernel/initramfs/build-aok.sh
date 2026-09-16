#!/bin/sh

set -eu

root_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
out_dir=${1:-"$root_dir/kernel/.build/aok-initramfs"}
work_dir="$out_dir/root"
archive="$out_dir/aok-initramfs.cpio.gz"

command -v cpio >/dev/null 2>&1 || { printf '%s\n' 'cpio is required.' >&2; exit 1; }
init_bin=${AOK_INIT_BINARY:-"$root_dir/runtime/.build/aok-init"}
supervisor_bin=${AOK_SUPERVISOR_BINARY:-"$root_dir/runtime/.build/aok-supervisor"}
manifest=${AOK_MANIFEST:-"$root_dir/manifests/research-agent.yaml"}
for file in "$init_bin" "$supervisor_bin" "$manifest"; do
	[ -f "$file" ] || { printf 'missing AOK initfs input: %s\n' "$file" >&2; exit 1; }
done

rm -rf "$work_dir"
mkdir -p "$work_dir/sbin" "$work_dir/etc/aok" "$work_dir/var/lib/aok" "$work_dir/dev" "$work_dir/proc" "$work_dir/sys" "$work_dir/tmp"
cp "$init_bin" "$work_dir/init"
cp "$supervisor_bin" "$work_dir/sbin/aok-supervisor"
cp "$manifest" "$work_dir/etc/aok/manifest.yaml"
chmod 0755 "$work_dir/init" "$work_dir/sbin/aok-supervisor"

(cd "$work_dir" && find . -print | cpio -o -H newc 2>/dev/null | gzip -9 > "$archive")
printf 'AOK_INITRAMFS=%s\n' "$archive"
