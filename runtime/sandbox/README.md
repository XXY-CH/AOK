# Linux process capability enforcement

`Policy.Args` compiles explicit rights to the `aok-sandbox` launcher ABI. The
launcher installs Landlock ABI 6 filesystem and process-domain restrictions,
sets `no_new_privs`, clears Linux process capabilities, installs seccomp, then
executes the absolute command path.
It exits 125 without executing the child when policy installation fails. The
launcher requires Linux arm64 or x86_64; there is no permissive macOS fallback.

```sh
cc -std=c11 -Wall -Wextra -Werror -O2 -static \
  runtime/sandbox/native/aok-sandbox.c -o /path/to/aok-sandbox
/path/to/aok-sandbox --read /input --write /output --keep-fd 3 -- /path/to/static-engine
sh runtime/sandbox/smoke.sh
```

Use a static engine, or separately grant read access to the trusted runtime
libraries required by a dynamic executable. The executable itself receives read
and execute rights. Additional executable files require `--execute PATH`.
`--read` and `--write` are separate: a write-only grant does not imply read.
The Go compiler accepts absolute paths and a terminal `/**` tree pattern;
unsupported globs and non-canonical paths fail closed.

All descriptors >= 3 are closed unless explicitly named by `--keep-fd N`
(3 through 63). A delegated descriptor is authority: the trusted supervisor must
pass only intended handles through `exec.Cmd.ExtraFiles`. Standard streams are
also trusted delegated descriptors. A pre-opened filesystem or network descriptor
can carry rights independent of Landlock path grants.

New sockets are denied by default. `--net` allows AF_INET and AF_INET6 socket
creation; AF_UNIX, socketpair and other address families remain denied. A delegated
Unix listener can serve supervisor IPC without granting arbitrary Unix socket
creation. Namespace, ptrace, cross-process memory/fd acquisition, mount, io_uring,
BPF, kernel module, device creation and filesystem metadata mutation syscalls are
denied. Landlock scopes signals and abstract sockets to the process domain.

This is the OS process boundary, not the complete `acap` object protocol. It does
not implement capability-fd revocation, URL/domain constraints, Biscuit/Cedar,
taint propagation or seccomp user notification. Policies are immutable after
launch; a supervisor must kill/restart a process to replace its filesystem rights.
URL-specific capabilities require a trusted network broker with raw network access
disabled in the child. Filesystem stat metadata is not hidden by Landlock.

## QEMU verification

The builder container kernel currently lacks usable Landlock. Its direct smoke
correctly exits 125. `build-qemu.sh` builds an independent kernel output directory
with Landlock and seccomp enabled; it never changes the existing AOK build output.

```sh
container exec aok-p1-builder sh /workspace/runtime/sandbox/build-qemu.sh
AOK_OBJECT_IMAGE="$PWD/kernel/.build/sandbox/Image" \
  AOK_INITRD="$PWD/kernel/.build/sandbox/initramfs.cpio.gz" \
  AOK_SERIAL_LOG="$PWD/kernel/.build/sandbox/serial.log" \
  AOK_TEST_MARKER=AOK_SANDBOX_TEST sh kernel/kselftest/run-object.sh
```

Override `AOK_SANDBOX_LINUX` with a prepared Linux source tree when the default
`/tmp/aok-p5-build/linux` is unavailable. `AOK_SANDBOX_BUILD` defaults to
`/tmp/aok-sandbox-build`; `AOK_SANDBOX_OUTPUT` defaults to `kernel/.build/sandbox`.
The QEMU helper tests real syscalls in a child process, including positive rights,
negative rights, symlink escape, metadata mutation, fd hygiene, explicit fd
delegation, network grants, Unix socket denial, fork inheritance and fail-closed
policy setup. The serial log must contain `AOK_SANDBOX_TEST=pass`.
