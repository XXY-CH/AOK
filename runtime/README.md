# AOK Runtime

This package implements an experimental user-space AOK engine and Unix socket
transport. `aok-engine-echo` is a standalone offline backend. `Encoder` and
`Decoder` provide newline-delimited JSON-RPC framing for requests, responses and
notifications. The wire specification remains `docs/abi/engine-protocol.md`;
this slice is not yet a complete implementation of Engine Protocol v1.

`Provider` receives a context and prompt and returns text plus usage. The offline
`EchoProvider` counts bytes for deterministic tests. `LlamaProvider` speaks the
native llama.cpp completion API and validates backend-reported usage. On Linux
arm64, `runtime/kernelbridge` binds the experimental AOK inference capability
ioctls; the QEMU probe exercises that ABI with a real llama.cpp CPU server.

`Engine` supports initialize, health, session/new, prompt, abort and close.
Events carry session_id, turn_id and a session-monotonic event_seq. Cancellation
is cooperative: abort requests cancellation; the prompt completion emits the
single terminal event after the provider returns. Close also cancels an active
turn and rejects later prompts. Providers must honor context cancellation.
Usage returned even on failure/cancellation is accounted. Prompt request replay
is method-scoped and returns the cached outcome with the current RPC ID.
Concurrent prompts for a busy session return an error; callers can retry later.

Each session has an independent Ledger. It uses saturating usage arithmetic,
monotonic sequence checks and last-usage-ID deduplication. Zero HardLimit means
unlimited in this local prototype (unlike the kernel U64_MAX convention).
This does not reserve model spend or enforce kernel quotas for the in-memory
engine. Durable application replay, checkpoint and mailbox state belongs to
`Supervisor`; `ProcessProvider` can run an engine as a separately supervised
child with restart-rate, timeout and sandbox limits.

## Process and transport

The supervisor must create a Unix stream socket in a private directory, restrict
socket permissions, and pass its listening fd to the engine. Linux guests may
use the equivalent AF_VSOCK listener from `ListenVsock`/`DialVsock`; the CID and
port remain supervisor-owned:

```sh
go build -o /tmp/aok-engine-echo ./cmd/aok-engine-echo
# Run only from a parent that has supplied the listener as inherited fd 3:
/tmp/aok-engine-echo -listener-fd 3
```

The engine never creates, removes or grants capability through a supplied path.
`Serve` owns its listener and requires a provider factory so each connection has
an independent engine. Initialize must precede session methods. Responses use
the request ID; valid notifications do not receive responses. Prompt execution
is asynchronous, so the same connection can send abort while inference runs.
A prompt is established before dispatching the next request, including abort.
The engine emits events before the prompt response, using a single socket writer.
Closing the connection cancels its work; it does not retire an AOK Application.
SIGINT/SIGTERM close the listener and cancel active connections.

Transport bounds are 32 connections, 16 pending prompts per connection, 256
outbound messages, 4 MiB input frames and a 10-second write deadline. Sessions
support bounded in-process `session/suspend` and `session/resume` event replay.
An outbound overflow suspends the session instead of dropping the connection:
refused events stay retained and `session/resume` replays them in `event_seq`
order. Responses carry no sequence number and cannot be replayed, so a response
overflow still closes the connection. Per-session history caps multiply, so the
engine also bounds their sum and reclaims from the largest holder. Delivery is
still not durable across processes, and a provider that ignores cancellation can
stall its own request until the supervisor timeout.

A development supervisor/client probe creates a private socket, passes its fd
to the compiled engine, exercises two turns and cleans up the child and files:

```sh
python3 scripts/smoke.py
```

It requires Go and Python 3 and uses no model service or credentials.

Validation:

```sh
go test -race ./...
go vet ./...
```

## Durable LSFS wake (user-space v0)

The supervisor binds LSFS artifact commits to an Application mailbox. Bindings,
scan cursors and mailbox records survive restart; each scan commits its cursor
and deliveries in one supervisor SQLite write. Artifact commits live in the
context database first, so a crash before scanning leaves them available for
replay. Event keys deduplicate rescans. Each Application supports up to 64
bindings, with up to 64 commits scanned per binding per runner iteration.

Use `aokctl -socket PATH METHOD JSON_PARAMS` with these control methods:

- `event_source.create`: `application_id`, `source_kind: "lsfs"`, `binding_id`,
  optional `source_ref` (defaults to the Application context), `after_cursor`
  (defaults to 0, replaying existing artifact commits).
- `event_source.list`: `application_id`, `source_kind: "lsfs"` returns binding
  snapshots. Omitting `source_kind` keeps the existing timer list response.
- `event_source.disable` / `event_source.bind`: `application_id`,
  `source_kind: "lsfs"`, `binding_id` pauses/resumes scanning from the saved cursor.
- `artifact.import`: `application_id`, `idempotency_key`, JSON `payload` commits
  an artifact to that Application's context and returns its content-hash `handle`.

Source contexts must belong to the target Application's owner. The local
control socket remains a same-host-user administrative API, not a multi-tenant
authorization boundary. The mailbox event carries commit metadata and a text
notification for the provider; it does not inject artifact contents into prompts.
The `lsfs:` and `timer:` mailbox key prefixes are reserved for source delivery.

Only artifact commits wake Applications in this slice. Runner append/checkpoint
writes advance the cursor without generating more work. Disabled bindings retain
their cursor; manual/frozen Applications accumulate pending events without
executing; retirement disables bindings and expires unacknowledged messages.
Provider execution remains serial and can delay scanning. Mailbox/commit
retention and aggregate disk quotas are not implemented, and this does not
implement kernel amem, port/poll wake or durable
Engine Protocol replay. Run `python3 scripts/core-smoke.py` to exercise process
crash recovery and headless timer/LSFS execution using the offline Echo provider.

### Mailbox admission and replay

Pending and claimed messages share a per-Application limit of 256 messages and
2 MiB of payload, plus a supervisor-wide limit of 1024 messages and 16 MiB.
Bytes are charged after JSON compaction and HTML escaping, matching the durable
representation across restart. The original send/timer input limit stays 256 KiB;
an accepted timer remains deliverable if persistence expands its JSON encoding.
`mailbox.capacity` with `application_id` returns usage and limits for both scopes.
These are outstanding-work limits, not total retained-memory or disk quotas:
acked/expired payloads, results, commits and audit records are still retained.

`message.send` returns RPC error `-32005` when capacity is exhausted. Retry with
the same key/payload after acknowledgements free capacity; an already accepted
idempotent retry succeeds even when full. Claiming work does not free capacity.
Successful ack, runner completion or retirement releases it; failed persistence
does not. Existing databases above the limits can drain normally but reject new
admissions until capacity is available.

LSFS delivery stops before advancing past an artifact that cannot be enqueued.
Timer delivery keeps the original due time and event identity until accepted.
Neither condition terminates the runner. Source state survives restart, so a
later scan retries after capacity is freed. Periodic timers preserve their
existing coalescing behavior: one deferred occurrence is delivered, then the
next due time is acceptance time plus the interval; missed ticks are not expanded.
