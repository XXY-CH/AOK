#!/bin/sh
set -eu
test "$(uname -s)" = Linux || { echo 'Linux is required' >&2; exit 1; }
source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
cc -std=c11 -Wall -Wextra -Werror -O2 -static "$source_dir/native/aok-sandbox.c" -o "$work/aok-sandbox"
cc -std=c11 -Wall -Wextra -Werror -O2 -static "$source_dir/native/probe.c" -o "$work/probe"
mkdir "$work/input" "$work/output" "$work/secret"
touch "$work/input/data" "$work/secret/data"
ln -s "$work/secret/data" "$work/input/escape"
"$work/aok-sandbox" --read "$work/input" --write "$work/output" -- "$work/probe" \
    "$work/input/data" "$work/output/data" "$work/secret/data" "$work/input/escape" 9<"$work/secret/data"
"$work/aok-sandbox" --net --keep-fd 9 --read "$work/input" --write "$work/output" -- "$work/probe" \
    "$work/input/data" "$work/output/data" "$work/secret/data" "$work/input/escape" network 9<"$work/secret/data"
if "$work/aok-sandbox" --read "$work/missing" -- "$work/probe" >/dev/null 2>&1; then
    echo 'FAIL invalid policy did not fail closed' >&2
    exit 1
fi
echo AOK_SANDBOX_FAIL_CLOSED_OK
