# AOK - Agent-native SubOS

**中文**

AOK 是运行在 macOS Apple Silicon 之上的 Agent-native SubOS。每个 SubOS 实例运行在独立的 Linux microVM 中，提供面向 Agent runtime 的资源域、上下文管理、推理后端和 capability 边界。AOK 的原生用户是 Agent runtime；CLI、TUI、GUI、ACP 和 Linux/POSIX 用户态属于外部控制或迁移适配层。

**English**

AOK is an agent-native SubOS for macOS on Apple Silicon. Each SubOS instance runs in an isolated Linux microVM and provides resource domains, context management, inference backends, and capability boundaries for agent runtimes. The native AOK client is an agent runtime; the CLI, TUI, GUI, ACP, and Linux/POSIX layers are external control or migration adapters.

架构与 syscall 规范 / Architecture and syscall specifications: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)

## 核心能力 / Core capabilities

- AOK Linux kernel patch series：object handles、root capability、aproc/task/pidfd、resource accounting、event sources 和 inference capability。
- Durable Go supervisor：SQLite WAL state、application lifecycle、mailbox replay、timer、checkpoint、context CAS 和 audit hash chain。
- Inference backends：llama.cpp、Anthropic Messages API、loopback Metal llama.cpp adapter，以及 supervisor-owned router/fallback。
- Runtime enforcement：Linux cgroup v2 `cpu.max`/`memory.max`、RSS 超限 `memory.reclaim`、signed attenuable capability tokens、Cedar-style deny-overrides、Landlock/seccomp sandbox。
- Guest bootstrap：`aok-init` PID1 基础入口、restart intensity、signal forwarding，以及可打包的 AOK initfs。
- Host interaction：Swift `aok` onboarding、TUI、settings 和 supervisor control API。

## 当前状态 / Current status

当前已验证的核心闭环包括：QEMU arm64 AOK kernel tests、real llama.cpp CPU probe、durable supervisor、独立 engine process、runtime resource enforcement、KV save/restore、Metal adapter、Anthropic adapter、router、vsock API、PID1/initfs 和 capability sandbox。

The validated core currently includes QEMU arm64 AOK kernel tests, a real llama.cpp CPU probe, the durable supervisor, supervised engine processes, runtime resource enforcement, KV save/restore, a Metal adapter, an Anthropic adapter, routing, the vsock API, PID1/initfs, and capability sandboxing.

仍有明确环境或内核边界：本机未配置 `ANTHROPIC_API_KEY`，因此没有真实 Anthropic service verification；当前 QEMU 没有可用 virtio-vsock device model；AOK 内核原生 sched_ext/memcg、amem/LSFS、全系统污点/unotify/witness 仍未完成。

Known boundaries remain explicit: `ANTHROPIC_API_KEY` is not configured on the development machine, so live Anthropic service verification is blocked; the current QEMU setup has no usable virtio-vsock device model; native AOK sched_ext/memcg enforcement, amem/LSFS, and system-wide taint/unotify/witness integration are still pending.

详细边界 / Detailed status: [docs/IMPLEMENTATION-STATUS.md](docs/IMPLEMENTATION-STATUS.md) 和 [docs/AOK-CORE-VALIDATION.md](docs/AOK-CORE-VALIDATION.md)

## 快速开始 / Quick start

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
make aok-core-test
```

完整 Linux builder/QEMU 步骤 / Full Linux builder and QEMU instructions: [kernel/kselftest/README.md](kernel/kselftest/README.md)

### Host TUI

```sh
swift run aok onboard
swift run aok tui
swift run aok settings show
swift run aok settings set engine echo
```

The TUI uses `aokctl` and the supervisor control API. It does not hold the kernel root capability.

### PID1 initfs

```sh
(cd runtime && go build -o .build/aok-init ./cmd/aok-init)
(cd runtime && go build -o .build/aok-supervisor ./cmd/aok-supervisor)
make aok-initramfs
```

The generated archive contains `/init`, `/sbin/aok-supervisor`, `/etc/aok/manifest.yaml`, and `/var/lib/aok`.

## Repository layout

| Path | 中文 | English |
| --- | --- | --- |
| `host/` | Swift CLI、TUI 和 VM/control adapter | Swift CLI, TUI, and VM/control adapters |
| `kernel/` | Linux baseline、AOK patches、configs、kselftests | Linux baseline, AOK patches, configs, and kselftests |
| `runtime/` | Go supervisor、engine protocol、providers 和 sandbox | Go supervisor, engine protocol, providers, and sandbox |
| `manifests/` | capability manifest 示例 | capability manifest examples |
| `docs/` | 架构、ABI、验证和实现边界 | architecture, ABI, validation, and implementation boundaries |

## Contributing

请先阅读 [AGENTS.md](AGENTS.md)（如在项目工作区可见）以及相关 ABI 和验证文档。提交改动前运行与影响范围相称的测试，并在文档中区分已实现、已验证和环境阻塞项。

Read the relevant ABI and validation documents before changing the project. Run tests appropriate to the affected area and keep implemented, verified, and environment-blocked behavior clearly separated in documentation.

## License

AOK is licensed under the [GNU General Public License v3.0](LICENSE). See the full text in [LICENSE](LICENSE).
