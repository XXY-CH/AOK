#!/bin/sh
# P5 user-space slice: queue research turns and retain their report.
#
#   scripts/mvp-research.sh [-j N] [-o output-directory] "research question"
#
# Against a running supervisor (AOK_CTL_SOCKET) or a freshly started one:
# planner turn derives the brief, N researchers fan out under VTC ordering
# with prefix affinity, the aggregator commits the report checkpoint to the
# context store, and the run prints measurements (report path, VTC
# ledger, cache hit rates, audit summary).
set -eu
umask 077
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
jobs=3
question="What limits current agent runtimes?"
output=${AOK_MVP_OUTPUT:-}
while [ $# -gt 0 ]; do
    case "$1" in
    -j) jobs=$2; shift 2 ;;
    -o) output=$2; shift 2 ;;
    -*) echo "usage: $0 [-j N] [-o output-directory] [question]" >&2; exit 2 ;;
    *) question=$1; shift ;;
    esac
done
case "$jobs" in ''|*[!0-9]*) echo "invalid researcher count" >&2; exit 2 ;; esac
[ "$jobs" -ge 1 ] && [ "$jobs" -le 16 ] || { echo "researcher count must be 1..16" >&2; exit 2; }
if [ -z "$output" ]; then
    mkdir -p "$repo/runtime/.build/research"
    output=$(mktemp -d "$repo/runtime/.build/research/run.XXXXXX")
else
    mkdir -p "$output"
fi
output=$(CDPATH= cd -- "$output" && pwd)
[ ! -e "$output/report.txt" ] && [ ! -e "$output/receipt.json" ] || {
    echo "output already contains a report; choose a new directory" >&2; exit 2;
}

work=$(mktemp -d "${TMPDIR:-/tmp}/aok-research.XXXXXX")
spid=
cleanup() {
    if [ -n "$spid" ]; then
        kill "$spid" 2>/dev/null || true
        wait "$spid" 2>/dev/null || true
    fi
    rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Reuse a running supervisor when its socket is provided.
if [ -n "${AOK_CTL_SOCKET:-}" ]; then
    [ -S "$AOK_CTL_SOCKET" ] || { echo "control socket does not exist" >&2; exit 1; }
    sock="$AOK_CTL_SOCKET"
else
    (cd "$repo/runtime" && go build -o "$work/supervisor" ./cmd/aok-supervisor)
    printf 'engine: echo\ncapabilities:\n  net: false\n' > "$output/manifest.yaml"
    "$work/supervisor" -state "$output/state" -manifest "$output/manifest.yaml" \
        -max-tokens 64 -parallel-turns "${AOK_MVP_PARALLEL:-3}" >"$output/supervisor.log" 2>&1 &
    spid=$!
    sock="$output/state/control.sock"
    i=0
    while [ $i -lt 150 ]; do
        [ -S "$sock" ] && break
        sleep 0.1
        i=$((i + 1))
    done
    [ -S "$sock" ] || { echo "supervisor did not become ready" >&2; exit 1; }
fi

AOK_CTL_SOCKET="$sock" AOK_MVP_RESEARCHERS="$jobs" \
    AOK_MVP_QUESTION="$question" AOK_MVP_OUTPUT="$output" \
    python3 "$repo/runtime/scripts/mvp_flow.py"
