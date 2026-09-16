#!/bin/sh
# SPDX-License-Identifier: GPL-2.0
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
candidate=${1:-}
baseline=${AOK_BASELINE:-f6388029ea9e2c9e807d73827658738ea131faee}
series=${AOK_PATCH_SERIES:-"$repo_root/kernel/patches/series"}
work=${AOK_CANDIDATE_CHECK_DIR:-"${TMPDIR:-/tmp}/aok-candidate-check-$$"}

test -n "$candidate" || { echo "usage: $0 PATCH" >&2; exit 2; }
case "$candidate" in
	/*) candidate_path=$candidate ;;
	*) candidate_path="$repo_root/$candidate" ;;
esac
test -f "$candidate_path"
test ! -e "$work"
test "$(git -C "$repo_root/kernel/linux" rev-parse HEAD)" = "$baseline"
mkdir -p "$work/source"
git -C "$repo_root/kernel/linux" archive "$baseline" | tar -x -C "$work/source"

while IFS= read -r name || [ -n "$name" ]; do
	case "$name" in ''|'#'*) continue ;; esac
	git -C "$work/source" apply "$repo_root/kernel/patches/$name"
done < "$series"

git -C "$work/source" apply --check "$candidate_path"
git -C "$work/source" apply --numstat "$candidate_path" | awk -F '\t' '{
	path = $3
	if (path ~ /^\/|(^|\/)\.\.($|\/)/ ||
	    path !~ /^(arch|block|certs|crypto|drivers|fs|include|init|ipc|kernel|lib|mm|net|security|scripts|virt)\//) {
		print "candidate touches disallowed path: " path > "/dev/stderr"; bad = 1
	}
} END { exit bad }'
if test "${AOK_CHECKPATCH:-0}" = 1 && command -v perl >/dev/null 2>&1 && test -x "$work/source/scripts/checkpatch.pl"; then
	perl "$work/source/scripts/checkpatch.pl" --no-tree --strict --ignore FILE_PATH_CHANGES \
		"$candidate_path"
fi
git -C "$work/source" apply "$candidate_path"
printf 'AOK_CANDIDATE_OK=1\n'
printf 'PATCH=%s\n' "$candidate_path"
printf 'BASELINE=%s\n' "$baseline"
printf 'WORK=%s\n' "$work"
