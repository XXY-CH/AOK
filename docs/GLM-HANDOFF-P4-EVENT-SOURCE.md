# GLM implementation handoff: 0005 event source

0005 adds a minimal out-of-tree event-source ABI on top of 0001-0004. Sources
are bound to an aproc fd and expose ordered timer/port records with explicit
ack, replay and overflow inspection. The patch is intentionally absent from
`series` until candidate, build and QEMU checks pass.
