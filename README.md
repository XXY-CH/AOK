# AOK — An OS That Truly Belongs to Agents

**English** | [简体中文](README.zh-CN.md)

> **Mission.** Today's agents are forced to operate computers the way humans do —
> screenshots, mouse clicks, and on-screen pixels. AOK exists to let agents abandon
> the clumsy Computer Use paradigm and embrace an OS that truly belongs to them:
> agent-native processes, capability-scoped resources, first-class context, and
> inference as a kernel device.
>
> **Stage.** AOK is currently in the **prototype-validation stage**: every claim in
> this repository is backed by reproducible tests or explicitly listed as
> environment-blocked. A fully independent, standalone OS release is being planned.

AOK is an agent-native SubOS for macOS on Apple Silicon. Each SubOS instance runs
in an isolated Linux microVM and provides resource domains, context management,
inference backends, and capability boundaries for agent runtimes. The native AOK
client is an agent runtime; the CLI, TUI, GUI, ACP, and Linux/POSIX layers are
external control or migration adapters.

Architecture and syscall specifications: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)

## Why not Computer Use?

Computer Use asks an agent to drive a human desktop through screenshots and
synthesized input. It is slow, expensive, unreliable, and unauditable — a
human-computer interface bolted onto a non-human user. AOK inverts the
relationship:

| | Computer Use | AOK |
| --- | --- | --- |
| Interface | pixels, screenshots, mouse/keyboard | typed fd/handle ABI |
| Process identity | a browser tab or GUI session | `aproc` with durable AID |
| Resources | invisible, unaccounted | CPU/memory/token budgets per agent |
| Context | scraped from the screen | context address space, KV snapshots |
| Inference | an API call from application code | `ainf` inference device with routing |
| Security | trust the whole desktop | signed, attenuable capabilities |
| Auditability | screen recordings | event ring, usage ledger, hash chain |

## Positioning

Existing efforts fall into two camps. Metaphor OSes (AIOS, Letta, OpenFang) model
agents in a userspace harness with no isolation and no kernel enforcement point.
Sandbox providers (E2B, Daytona, microsandbox, K8s agent sandboxes) offer real
isolation but treat the agent as untrusted code with no agent semantics. AOK
occupies the intersection:

- **True isolation**: each SubOS instance is a lightweight Linux microVM with its
  own kernel, based on the Apple Containerization framework.
- **Agent-native kernel**: kernel abstractions (process, scheduling, context,
  IPC, capability) target LLM agents directly; Linux tasks, POSIX, and desktop
  interaction are implementation or external-adapter details.
- **Pluggable engines**: a sub-agent engine (model runtime) is just another kind
  of process inside the kernel; the kernel does not care which model it runs.
- **A growth environment for agents**: an Application is a composable unit of
  capability and state, created, upgraded, hibernated, and retired on demand;
  human interfaces are an optional control plane, not a survival condition.

## Layering

```
macOS 27 host
└─ H   aok-host (Swift, Containerization framework)
       · VM lifecycle, resource quotas, pause/resume, hostfs bridge, connectors
└─ L0  AOK Linux kernel fork + in-VM PID 1 supervisor
       · aproc/AID, CPU/memory/token resource domains, sched_ext, freezer,
         capability enforcement, AOK syscall surface (agent-native fd objects)
└─ L1  AOK runtime/libOS (Go, userspace)
       · agent session, turn, context/KV snapshot, supervision policy,
         router system agent, web capability stack, message gateway
└─ L2  Agent worker / engine
       · engine trait, echo-engine, llama.cpp / Metal / Anthropic backends,
         manifest-compiled Landlock + seccomp sandbox
```

An Application does not belong to a single Linux task. It consists of durable
identity, a versioned contract, memory, and tool bindings, executed by one or
more `aproc` incarnations. Workers can crash, freeze, or be replaced while
Application identity, unconfirmed messages, and checkpoints persist. Dynamic
model routing is performed by the L1 router system agent; the kernel enforces
capability, resource domains, cache compatibility, and usage reconciliation.

## Core capabilities

- **AOK Linux kernel patch series** (`0001`–`0010`, on top of `linux-6.18.y`):
  object handles, PID1 root capability bootstrap, aproc/task/pidfd lifecycle,
  resource budget narrowing with CPU/RSS observation and token accounting,
  timer/port event sources with poll, ack/replay, authorized aproc wake, the
  inference capability, and the within-boot durable application registry (LSFS
  event source, per-application ack isolation, snapshot/restore).
- **Durable Go supervisor**: SQLite WAL state, application lifecycle, mailbox
  replay, timers, checkpoints, context CAS, and an audit hash chain.
- **Inference backends**: llama.cpp, the Anthropic Messages API, a loopback
  Metal llama.cpp adapter, and a supervisor-owned router/fallback chain.
- **Runtime enforcement**: Linux cgroup v2 `cpu.max`/`memory.max`, RSS-triggered
  `memory.reclaim`, signed attenuable capability tokens, Cedar-style
  deny-overrides, Landlock/seccomp sandboxing.
- **Guest bootstrap**: `aok-init` PID1 entry, restart intensity, signal
  forwarding, and a packageable AOK initfs.
- **Host interaction**: Swift `aok` onboarding, TUI, settings, and the
  supervisor control API. CLI/GUI never holds the kernel root capability.

## Current status — prototype validation

The validated core loop includes: QEMU arm64 AOK kernel tests, a real llama.cpp
CPU probe through the kernel inference ABI, the durable supervisor with crash
replay, supervised engine processes, runtime resource enforcement, KV
save/restore, the Metal adapter, the Anthropic adapter, routing, the vsock API,
PID1/initfs, and the capability sandbox.

Kernel-side validation totals for the enabled build are: object 44, task 64,
resource 53, event-source 38, event-wake 37, app-registry 45, and core 46
kselftests passing. The disabled build separately passes object 4, task 11,
resource 8, event-source 6, and app-registry 5 `ENOSYS` checks, with
reproducible patch-series rebuilds.

Known boundaries remain explicit: `ANTHROPIC_API_KEY` is not configured on the
development machine, so live Anthropic service verification is blocked; the
current QEMU setup has no usable virtio-vsock device model; native AOK
sched_ext/memcg enforcement, amem/LSFS storage, and cross-process engine
session replay are still pending. The runtime provides bounded in-process
session suspend/resume, durable Application generation/checkpoint/mailbox
recovery, and the kernel application-registry drain/snapshot/restore wiring.

Detailed status: [docs/IMPLEMENTATION-STATUS.md](docs/IMPLEMENTATION-STATUS.md),
[docs/AOK-CORE-VALIDATION.md](docs/AOK-CORE-VALIDATION.md),
[docs/KERNEL-EVENT-WAKE-VALIDATION.md](docs/KERNEL-EVENT-WAKE-VALIDATION.md), and
[docs/KERNEL-APP-REGISTRY-VALIDATION.md](docs/KERNEL-APP-REGISTRY-VALIDATION.md)

## Roadmap

1. **Prototype validation** *(current stage)* — agent-native kernel primitives
   (`aproc`, resource domains, event sources, inference capability) proven in
   QEMU arm64, with a durable supervised runtime loop.
2. **Capability model completion** — manifest-compiled Landlock/seccomp/acap
   enforcement across engines and applications.
3. **Persistent context** — amem context virtual memory and LSFS memory storage
   (git semantics over a single SQLite store), durable event sources and wake.
4. **Agent scheduling and inference devices** — sched_ext turn-level preemptive
   scheduling with token fairness; `ainf` kernel device with pluggable backends.
5. **Standalone OS** *(planning)* — an independent, self-contained OS release
   that no longer depends on a host-adapted Linux baseline.

## Quick start

### Linux baseline

```sh
make linux-fetch
make linux-status
```

### Runtime validation

```sh
make runtime-test       # go test -race ./... && go vet ./...
make runtime-smoke      # independent engine smoke test
make runtime-core-smoke # durable supervisor crash replay and timer smoke
```

### AOK kernel tests

```sh
make aok-object-build
make aok-object-test
make aok-task-test
make aok-resource-test
make aok-eventsrc-test
make aok-eventwake-test
make aok-appregistry-test
make aok-event-probe-test
make aok-core-test
```

Full Linux builder and QEMU instructions: [kernel/kselftest/README.md](kernel/kselftest/README.md)

### Host TUI

```sh
swift run aok onboard
swift run aok tui
swift run aok settings show
swift run aok settings set engine echo
```

The TUI uses `aokctl` and the supervisor control API. It does not hold the
kernel root capability.

### PID1 initfs

```sh
(cd runtime && go build -o .build/aok-init ./cmd/aok-init)
(cd runtime && go build -o .build/aok-supervisor ./cmd/aok-supervisor)
make aok-initramfs
```

The generated archive contains `/init`, `/sbin/aok-supervisor`,
`/etc/aok/manifest.yaml`, and `/var/lib/aok`.

## Repository layout

| Path | Contents |
| --- | --- |
| `host/` | Swift CLI, TUI, and VM/control adapters |
| `kernel/` | Linux baseline, AOK patches, configs, kselftests |
| `runtime/` | Go supervisor, engine protocol, providers, sandbox |
| `manifests/` | capability manifest examples |
| `docs/` | architecture, ABI, validation, and implementation boundaries |

## Documentation

| Document | Contents |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | layering, application model, ABI boundaries |
| [docs/IMPLEMENTATION-STATUS.md](docs/IMPLEMENTATION-STATUS.md) | frozen decisions and current boundaries |
| [docs/KERNEL-EVENT-WAKE-VALIDATION.md](docs/KERNEL-EVENT-WAKE-VALIDATION.md) | kernel timer/port poll and aproc wake evidence |
| [docs/KERNEL-APP-REGISTRY-VALIDATION.md](docs/KERNEL-APP-REGISTRY-VALIDATION.md) | kernel application registry, durable replay and supervisor wiring evidence |
| [docs/PLAN-L0-AGENT-KERNEL.md](docs/PLAN-L0-AGENT-KERNEL.md) | L0 agent-kernel design plan |
| [docs/PLAN-AOK-DEEP.md](docs/PLAN-AOK-DEEP.md) | deep design decisions with research sources |
| [docs/abi/](docs/abi/) | fd ABI, engine protocol, control API, application/context models |
| `docs/*-VALIDATION.md` | per-phase QEMU and runtime validation evidence |

## Contributing

Read the relevant ABI and validation documents before changing the project.
Run tests appropriate to the affected area and keep implemented, verified, and
environment-blocked behavior clearly separated in documentation.

## License

AOK is licensed under the [GNU General Public License v3.0](LICENSE).
