# AOK kselftest

`object.c` uses the upstream kselftest TAP helpers to exercise real AOK syscalls
from the initial PID 1 of an arm64 QEMU guest. It is a test init, not the AOK
supervisor. No mock or userspace replacement implements these calls.

The 44 checks cover identity, argument sizing, invalid addresses/fds, rights
attenuation, fork/exec, PID namespace denial, fd exhaustion, copyout rollback,
and 4000 concurrent duplicate/inspect/close cycles. The same binary verifies
four `ENOSYS` results with `CONFIG_AOK_EXPERIMENTAL=n`.

## Build and Run

Build on Linux arm64 with GNU Make >= 4.0, GCC, binutils, flex, bison, bc,
libelf-dev, libssl-dev, rsync, cpio, gzip and git. Debian bookworm's
`build-essential` supplies the compiler and static glibc used here.

```sh
# Linux builder, from repository root. AOK_OBJECT_BUILD must be a fresh directory.
make aok-object-build
# macOS with QEMU installed, or Linux with QEMU:
make aok-object-test
make aok-object-test-disabled
```

On macOS, a Linux builder can access the repository using Apple Container:

```sh
container run -d --name aok-p1-builder --cpus 6 --memory 6G \
  -v "$PWD:/workspace" -v "$PWD/kernel/linux:/workspace/kernel/linux" \
  debian:bookworm-slim sleep infinity
container exec aok-p1-builder sh -c \
  'apt-get update && apt-get install -y --no-install-recommends build-essential flex bison libelf-dev libssl-dev bc rsync cpio git'
container exec aok-p1-builder sh -c \
  'cd /workspace && make aok-object-build > kernel/.build/object-build.log 2>&1'
make aok-object-test
make aok-object-test-disabled
container stop aok-p1-builder
```

The build exports the pinned baseline to a separate Linux directory and applies
`kernel/patches/series` there. It never configures or patches `kernel/linux`.
The output defaults to `kernel/.build/qemu-arm64-object`: enabled/disabled kernel
images, exported UAPI headers, initramfs, configs, hashes and serial logs. Override
`AOK_OBJECT_OUTPUT` and `AOK_OBJECT_BUILD` to keep additional runs separately.

The runner fails on timeout, panic, Oops, warning, failing TAP results or a missing
`AOK_OBJECT_TEST=pass` marker. This is object-slice evidence only; task binding,
resource domains and event sources need their own tests before integration.

`eventwake.c` exercises source polling, bounded port notifications and authorized
timer/port wake of a frozen aproc using a real child-task heartbeat. It also checks
manual policy, detach, resource freeze, terminal states and repeated freeze/wake.
`make aok-object-build` packs it into `eventwake-initramfs.cpio.gz`; run
`make aok-eventwake-test` (optionally with `AOK_OBJECT_OUTPUT` for a separate build).
See [event wake validation](../../docs/KERNEL-EVENT-WAKE-VALIDATION.md) for results
and the boundary between this volatile kernel source and the durable supervisor.

`appregistry.c` exercises the `0010` application registry: idempotent
registration, LSFS posts with a commit cursor, the shared id/sequence space,
replay across source fd release and across processes, application fd read/poll/
ack rights, ack isolation per application, bounded-queue EAGAIN, snapshot and
restore round-trips and rebind cursor reset. `make aok-appregistry-test` and
`make aok-appregistry-test-disabled` run it; results and the mutation check are
in [application registry validation](../../docs/KERNEL-APP-REGISTRY-VALIDATION.md).
All `aok-*-test` targets honor `AOK_OBJECT_OUTPUT` to select a build directory.
