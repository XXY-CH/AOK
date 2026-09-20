#!/bin/sh
# P5 acceptance: one command drives parallel research end to end.
#
#   scripts/mvp-research.sh [-j N] "research question"
#
# Against a running supervisor (AOK_CTL_SOCKET) or a freshly started one:
# planner turn derives the brief, N researchers fan out under VTC ordering
# with prefix affinity, the aggregator commits the report checkpoint to the
# context store, and the run prints the acceptance data (report path, VTC
# ledger, cache hit rates, audit summary).
set -eu
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
jobs=3
question="What limits current agent runtimes?"
while [ $# -gt 0 ]; do
    case "$1" in
    -j) jobs=$2; shift 2 ;;
    -*) echo "usage: $0 [-j N] [question]" >&2; exit 2 ;;
    *) question=$1; shift ;;
    esac
done

work=$(mktemp -d "${TMPDIR:-/tmp}/aok-research.XXXXXX")
trap 'kill "$spid" 2>/dev/null || true; rm -rf "$work"' EXIT
(cd "$repo/runtime" && go build -o "$work/supervisor" ./cmd/aok-supervisor)
printf 'engine: echo\ncapabilities:\n  net: false\n' > "$work/manifest.yaml"

# Reuse a running supervisor when its socket is provided.
if [ -n "${AOK_CTL_SOCKET:-}" ] && [ -S "${AOK_CTL_SOCKET:-}" ]; then
    sock="$AOK_CTL_SOCKET"
    owns=0
else
    "$work/supervisor" -state "$work/state" -manifest "$work/manifest.yaml" \
        -max-tokens 64 >"$work/supervisor.log" 2>&1 &
    spid=$!
    sock="$work/state/control.sock"
    owns=1
    i=0
    while [ $i -lt 150 ]; do
        [ -S "$sock" ] && break
        sleep 0.1
        i=$((i + 1))
    done
    [ -S "$sock" ] || { echo "supervisor did not become ready" >&2; exit 1; }
fi

AOK_CTL_SOCKET="$sock" AOK_MVP_RESEARCHERS="$jobs" \
    AOK_MVP_QUESTION="$question" \
    python3 "$repo/runtime/scripts/mvp_flow.py"
