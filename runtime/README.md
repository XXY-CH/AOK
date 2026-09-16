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
outbound messages, 4 MiB input frames and a 10-second write deadline. An outbound
queue overflow closes the connection and cancels work. This is a prototype
failure policy, **not** v1 suspend/resume or durable delivery. A provider that
ignores cancellation can stall its own request until the supervisor timeout.

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
