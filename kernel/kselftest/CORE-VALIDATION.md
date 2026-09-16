# Inference/Capability Control Validation

Validated on 2026-09-16 with Linux 6.18.51 baseline
`f6388029ea9e2c9e807d73827658738ea131faee`, patches 0001 through 0006,
Debian arm64 GCC 12.2.0 in `aok-p1-builder`, and host QEMU 11.1.1.

`0006-aok-inference-capability.patch` passed the clean-baseline candidate
application check. The enabled kernel compiled successfully. The new PID1
test passed 46/46 assertions in QEMU. Existing object 44/44, task 64/64,
resource 50/50, and event-source 37/37 tests also passed on that same image.
The serial runner found no panic, BUG, WARNING, or failing TAP assertion.

The follow-up `0007-aok-token-budget-enforcement.patch` was then applied to
the same chain. Its resource test passed 51/51, including conservative
over-limit settlement, the FROZEN state event, and explicit resume; the
disabled resource test passed 8/8 with `ENOSYS`. The unchanged object/task/
event/core regressions passed 44/44, 64/64, 37/37, and 46/46.

## Reproduction

After 0006 is in `kernel/patches/series`, use the existing full builder:

```sh
container exec aok-p1-builder sh -lc \
  'AOK_OBJECT_BUILD=/tmp/aok-core-rebuild AOK_OBJECT_OUTPUT=/workspace/kernel/.build/qemu-arm64-core-rebuild sh /workspace/kernel/kselftest/build-object.sh'
AOK_OBJECT_OUTPUT="$PWD/kernel/.build/qemu-arm64-core-rebuild" \
AOK_INITRD="$PWD/kernel/.build/qemu-arm64-core-rebuild/core-initramfs.cpio.gz" \
AOK_TEST_MARKER=AOK_CORE_TEST sh kernel/kselftest/run-object.sh
```

This run used a private copy of the previously verified 0001-0005 source and
build output in `/tmp/aok-core-build` inside the builder. The clean candidate
check separately reconstructed the pinned upstream tree before applying all
patches. `kernel/linux` was not edited.

Evidence lives in `kernel/.build/qemu-arm64-core/`: `serial.log`,
`object-regression.log`, `task-regression.log`, `resource-regression.log`,
and `eventsrc-regression.log`.

SHA-256:

```text
patch: e7f6896013c461d5e229e72ecedea6cc4e42b447b0be0e25d4ef525b41560118
Image: eb9e3dfc956ffd2b9d62116a1f81f14f190415de94392e65f20550feb86b3cd6
core-initramfs: 277fb21a6317d0ad674796255a1901634e88cd871861edbce0272e3e7088545e
```

## ABI and Scope

The root-job fd exposes `AOK_CORE_CAP_CREATE` through `ioctl(2)`. Derived
capabilities can only remove rights, reduce token ceilings/export masks, and
add taint bits. Ancestor references keep the revocation chain alive after
parent fd closure. The chain is bounded to 32 derivations. Revocation is
immediate at the next ioctl/poll boundary and applies to descendants.

Only a capability with both `INFER` and `SERVE` can create a session pair.
The trusted supervisor retains `SERVE` authority and passes the client fd to
an engine and the backend fd to its trusted backend. A client cannot complete
requests; a backend cannot submit them. All minted fds have `FD_CLOEXEC`.
The fixed arm64 structures have sizes 32 bytes (`aok_core_cap`), 8 bytes
(`aok_core_session`), and 560 bytes (`aok_core_io`). No new syscall numbers
were allocated.

Each session accepts one outstanding request with a strictly increasing
sequence. Submit reserves its input/output ceilings against every ancestor,
so concurrent sessions and siblings share their ancestor's ceiling. Backend
take transitions pending to running only after successful copyout. Completion
commits backend-reported usage once and releases unused reservation. Pending
cancellation refunds the reservation; cancellation or endpoint closure after
take conservatively charges the reserved ceiling. `poll(2)` signals pending
backend work, client completion, cancellation, and revocation.

Kernel-owned taint monotonically accumulates over a session. `EXPORT` checks
the capability export mask and rejects disallowed labels. Ordinary `RESULT`
returns the payload to the application with its label. This is an explicit
controlled-export gate, not an LSM hook on every ordinary Linux network or
filesystem syscall. The surrounding process sandbox and broker must enforce
those I/O paths. This patch does not establish complete system-wide taint
enforcement, provenance for arbitrary user memory, Biscuit/Cedar policy,
human confirmation, or external audit witnesses.

The test backend supplies known bytes to validate transport/accounting. It is
not an actual model and is not evidence of llama.cpp inference, vsock, KV
save/restore, or host backend supervision. Payloads are bounded to 512 bytes;
streaming and mmap queues are not implemented. Accounting is bounded by a
trusted backend's declared actual usage, not independently measured model
hardware consumption. These capabilities are separate from the earlier aproc
token observation ledger. CONFIG-disabled core tests were not added because
there is no root handle or new syscall when AOK is disabled.

## Go PID1 and Actual Model Integration

The separate `runtime/cmd/aok-kernel-probe` has now booted as real guest PID1,
mounted proc/sys/dev, and used `runtime/kernelbridge` to claim the root handle,
derive capabilities, submit/take/complete inference, export a permitted result,
reject over-budget submit, and revoke live session authority through an ancestor.
The probe runs both deterministic echo and the actual `runtime.LlamaProvider`.

The second kernel configuration additionally enables EVENTFD (required by Go's
network poller), INET, VIRTIO_MMIO, and VIRTIO_NET. QEMU user networking maps
guest `10.0.2.2:18081` to host llama.cpp. The model was the existing
`stories15M-q4_0.gguf`, run on CPU with `-ngl 0`. The completed guest trace is
`kernel/.build/qemu-arm64-core/probe-serial.log`:

```text
AOK_KERNEL_PROVIDER=echo input=11 output=11 used=22 text="kernel echo"
AOK_KERNEL_PROVIDER=llama.cpp input=11 output=32 used=43
AOK_KERNEL_PROBE=pass
```

Actual llama.cpp text began ` shiny coin on the ground. She was so happy and
wanted to keep it forever.` The kernel result matched the backend bytes and
committed exactly 43 reported tokens. The full probe passed without kernel
warnings or panic. This is actual model inference through the kernel fd ABI;
it uses TCP over QEMU slirp, not vsock or Metal. The backend adapter runs in
the PID1 probe process, so this does not verify separate backend supervision.
Host model availability and an explicit endpoint are required; no fallback is
used when an endpoint was requested.

```sh
container exec aok-p1-builder sh /workspace/kernel/kselftest/build-core-network.sh
llama-server -m kernel/.build/stories15M-q4_0.gguf \
  --host 127.0.0.1 --port 18081 -c 2048 -ngl 0 --parallel 1
# In another terminal:
AOK_OBJECT_IMAGE="$PWD/kernel/.build/qemu-arm64-core/Image.network" \
AOK_QEMU_NETWORK=1 AOK_TEST_ARGS='aok.llama=http://10.0.2.2:18081' \
  sh runtime/scripts/kernel-probe.sh
```

The network builder defaults to the existing private `/tmp/aok-core-build`;
set `AOK_CORE_BUILD` to a fresh full build directory for reproduction.
Go race tests cover the unsupported-platform fail-closed path, and the Go
binding/probe pass Linux arm64 cross-target `go vet`.

```text
network Image: e210b7353cfd6e1b77c52287529392e9dd907c8995bf838f55018d4fdb0f1248
probe initramfs: d6b670ace084415607758f6db0a2a98ea3eeaf68cd09cb01b5f8450aec8c9107
GGUF: 66967fbece6dbe97886593fdbb73589584927e29119ec31f08090732d1861739
```
