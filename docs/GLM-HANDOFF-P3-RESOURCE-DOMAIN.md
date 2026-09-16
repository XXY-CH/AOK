# GLM implementation handoff: 0004 resource domain

Implement the next out-of-tree patch on top of `0001`, `0002`, and `0003`.
The patch must remain applicable to Linux `f6388029ea9e2c9e807d73827658738ea131faee`.

## Required ABI

Add budget state to `struct aok_aproc` and expose the following calls:

- `aok_budget_set(aproc_fd, struct aok_budget __user *)`
- `aok_budget_get(aproc_fd, struct aok_budget_status __user *)`
- `aok_token_usage(aproc_fd, struct aok_token_usage __user *)`

Use new syscall numbers after 480 and update both generic syscall tables and `sys_ni.c`.
All structures have a `size` field and reserved zero fields; reject undersized input and preserve
forward-compatible tail fields.

## Semantics

Budgets are monotonic restrictions. A child aproc may only receive limits no greater than its parent.
CPU usage is sampled from task accounting, memory usage from the aproc task set's memcg, and token
usage from reports bound to the aproc. Counters use saturating arithmetic and never wrap around.

Token reports contain an unrepeatable `usage_id` and monotonic `usage_seq`; duplicates are idempotent,
out-of-order reports are rejected, and missing/untrusted reports charge the reserved upper bound.

Each resource independently progresses through `within -> throttle -> freeze -> supervisor_event`.
The default terminal action is an event; OOM kill remains opt-in. Reuse the existing aproc event ring
and ensure resource events cannot bypass capability checks.

## Concurrency and testing

Do not hold `aproc->lock` while sleeping or querying memcg. State transitions must be rechecked after
sampling. Budget updates, usage reconciliation, freeze and abort must not double-acquire or release
the freezer static key. Add kselftests for limits, rights, inheritance, duplicate/乱序 usage reports,
saturating counters, out-of-order usage reports, each enforcement level, disabled `ENOSYS`, and regression of the existing 64 task
tests. Record enabled/disabled builds and four QEMU suites in `kernel/.build/p4-report.md`; do not add
the patch to `series` until the complete chain passes.
