#!/bin/sh
# SPDX-License-Identifier: GPL-2.0
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
baseline=${AOK_BASELINE:-f6388029ea9e2c9e807d73827658738ea131faee}
series=${AOK_PATCH_SERIES:-"$repo_root/kernel/patches/series"}
work=${AOK_PATCH_CHECK_DIR:-"${TMPDIR:-/tmp}/aok-patch-check-$$"}

test "$(git -C "$repo_root/kernel/linux" rev-parse HEAD)" = "$baseline"
test ! -e "$work"
mkdir -p "$work/source"
git -C "$repo_root/kernel/linux" archive "$baseline" | tar -x -C "$work/source"
applied=0
while IFS= read -r name || [ -n "$name" ]; do
	case "$name" in ''|'#'*) continue ;; esac
	test -f "$repo_root/kernel/patches/$name"
	git -C "$work/source" apply --check "$repo_root/kernel/patches/$name"
	git -C "$work/source" apply "$repo_root/kernel/patches/$name"
	applied=$((applied + 1))
done < "$series"
printf 'AOK_PATCH_SERIES_OK=%s\n' "$applied"
printf 'BASELINE=%s\n' "$baseline"
printf 'WORK=%s\n' "$work"
