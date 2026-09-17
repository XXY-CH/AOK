# AOK — 真正属于 Agent 的操作系统

[English](README.md) | **简体中文**

> **使命。** 今天的 Agent 被迫用人类的方式操作计算机——截图、点击、
> 盯着屏幕上的像素。AOK 的存在，就是为了让 Agent 抛弃愚蠢的 Computer Use，
> 拥抱真正属于它的 OS：Agent 原生进程、能力约束的资源、一等公民的上下文、
> 以及作为内核设备的推理。
>
> **阶段。** AOK 目前处于**原型验证阶段**：本仓库中的每一项声明都有可复现的
> 测试支撑，或被明确标注为环境阻塞。完全独立、不依赖宿主适配的独立 OS
> 版本正在规划当中。

AOK 是运行在 macOS Apple Silicon 之上的 Agent-native SubOS。每个 SubOS 实例
运行在独立的 Linux microVM 中，提供面向 Agent runtime 的资源域、上下文管理、
推理后端和 capability 边界。AOK 的原生用户是 Agent runtime；CLI、TUI、GUI、
ACP 和 Linux/POSIX 用户态属于外部控制或迁移适配层。

架构与 syscall 规范：[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)

## 为什么不是 Computer Use？

Computer Use 要求 Agent 通过截图和合成输入去驱动人类桌面。它慢、贵、不可靠、
不可审计——把人机接口硬套在一个非人类的用户身上。AOK 反转了这层关系：

| | Computer Use | AOK |
| --- | --- | --- |
| 接口 | 像素、截图、鼠标/键盘 | 类型化 fd/handle ABI |
| 进程身份 | 一个浏览器标签页或 GUI 会话 | 持久 AID 的 `aproc` |
| 资源 | 不可见、无计量 | 每个 Agent 的 CPU/内存/token 预算 |
| 上下文 | 从屏幕上抓取 | 上下文地址空间、KV 快照 |
| 推理 | 应用代码里的一次 API 调用 | 带路由的 `ainf` 推理设备 |
| 安全 | 信任整个桌面 | 签名的、可衰减的 capability |
| 可审计 | 屏幕录像 | 事件环、usage 台账、hash chain |

## 生态定位

现有探索分成两个阵营。隐喻 OS（AIOS、Letta、OpenFang）在用户态 harness 里
建模 Agent，没有隔离，也没有内核强制点；沙箱服务（E2B、Daytona、
microsandbox、K8s agent sandbox）提供真隔离，但把 Agent 当不可信代码，
没有任何 Agent 语义。AOK 占据两者的交集：

- **真隔离**：每个 SubOS 实例是基于 Apple Containerization framework 的轻量
  Linux microVM，拥有独立内核。
- **Agent-native 内核**：内核抽象（进程、调度、上下文、IPC、能力）直接面向
  LLM Agent；Linux task、POSIX 和桌面交互只是实现或外部适配细节。
- **可插拔引擎**：SubAgent 引擎（模型 runtime）只是内核里的一种进程，内核
  不关心里面跑什么模型。
- **Agent 生长环境**：Application 是可组合的能力与状态单元，按需求创建、
  升级、休眠和退役；人类界面只是可选控制面，不是生存条件。

## 分层

```
macOS 27 宿主
└─ H   aok-host（Swift，Containerization framework）
       · VM 生命周期、资源配额、pause/resume、hostfs bridge、外部 connector
└─ L0  AOK Linux kernel fork + VM 内 PID 1 supervisor
       · aproc/AID、CPU/内存/token 资源域、sched_ext、freezer、
         capability enforcement、AOK syscall 面（Agent-native fd 对象）
└─ L1  AOK runtime/libOS（Go，用户态）
       · Agent session、turn、上下文/KV 快照、监督策略、
         router system agent、web capability stack、消息 gateway
└─ L2  Agent worker / engine
       · engine trait、echo-engine、llama.cpp / Metal / Anthropic 后端、
         manifest 编译的 Landlock + seccomp sandbox
```

Application 不属于某一个 Linux task。它由持久身份、版本化 contract、记忆与
工具绑定组成，由一个或多个 `aproc` incarnation 执行。worker 可以崩溃、冻结
或被替换，而 Application 身份、未确认消息和 checkpoint 持续存在。动态模型
路由由 L1 router system agent 执行；内核只执行 capability、资源域、缓存兼容
性和 usage 对账。

## 核心能力

- **AOK Linux kernel patch series**（`0001`–`0008`，基于 `linux-6.18.y`）：
  object handles、PID1 root capability bootstrap、aproc/task/pidfd 生命周期、
  资源预算收窄与 CPU/RSS 观测及 token 台账、带 ack/replay 的 timer 事件源、
  以及推理 capability。
- **Durable Go supervisor**：SQLite WAL 状态、application 生命周期、mailbox
  replay、timer、checkpoint、context CAS 和 audit hash chain。
- **推理后端**：llama.cpp、Anthropic Messages API、loopback Metal llama.cpp
  adapter，以及 supervisor 持有的 router/fallback 链。
- **运行时强制**：Linux cgroup v2 `cpu.max`/`memory.max`、RSS 触发的
  `memory.reclaim`、签名可衰减 capability token、Cedar 风格 deny-overrides、
  Landlock/seccomp sandbox。
- **Guest 引导**：`aok-init` PID1 入口、restart intensity、signal forwarding，
  以及可打包的 AOK initfs。
- **宿主交互**：Swift `aok` onboarding、TUI、settings 和 supervisor control
  API。CLI/GUI 永远不持有内核 root capability。

## 当前状态 — 原型验证

已验证的核心闭环包括：QEMU arm64 AOK kernel tests、经内核推理 ABI 的真实
llama.cpp CPU probe、带崩溃回放的 durable supervisor、被监督的 engine 进程、
运行时资源强制、KV save/restore、Metal adapter、Anthropic adapter、router、
vsock API、PID1/initfs 和 capability sandbox。

内核侧验证累计：object 44、task 64、event-source 37、resource 53、core 46
项 kselftest 在启用/禁用两种内核构建下通过，外加可复现的 patch series 重建。

已知边界保持显式：本机未配置 `ANTHROPIC_API_KEY`，因此没有真实 Anthropic
service verification；当前 QEMU 没有可用 virtio-vsock device model；AOK 内核
原生 sched_ext/memcg 强制、amem/LSFS、全系统污点/unotify/witness 和跨进程
engine session replay 仍未完成。runtime 已提供有界的进程内 session
suspend/resume，以及持久 Application generation/checkpoint/mailbox 恢复。

详细状态：[docs/IMPLEMENTATION-STATUS.md](docs/IMPLEMENTATION-STATUS.md) 和
[docs/AOK-CORE-VALIDATION.md](docs/AOK-CORE-VALIDATION.md)

## 路线

1. **原型验证**（当前阶段）——Agent-native 内核原语（`aproc`、资源域、
   事件源、推理 capability）在 QEMU arm64 中验证，配套 durable 的被监督
   runtime 闭环。
2. **能力模型补全**——manifest 编译的 Landlock/seccomp/acap 强制覆盖所有
   engine 与 application。
3. **持久上下文**——amem 上下文虚拟内存与 LSFS 记忆存储（单一 SQLite 之上
   的 git 语义）、durable 事件源与唤醒。
4. **Agent 调度与推理设备**——sched_ext turn 级抢占调度与 token 公平；
   可插拔后端的 `ainf` 内核设备。
5. **独立 OS**（规划中）——完全独立、自包含的 OS 版本，不再依赖宿主适配的
   Linux baseline。

## 快速开始

### Linux baseline

```sh
make linux-fetch
make linux-status
```

### Runtime 验证

```sh
make runtime-test       # go test -race ./... && go vet ./...
make runtime-smoke      # 独立 engine smoke test
make runtime-core-smoke # durable supervisor 崩溃回放与 timer smoke
```

### AOK kernel 测试

```sh
make aok-object-build
make aok-object-test
make aok-task-test
make aok-resource-test
make aok-eventsrc-test
make aok-core-test
```

完整 Linux builder/QEMU 步骤：[kernel/kselftest/README.md](kernel/kselftest/README.md)

### Host TUI

```sh
swift run aok onboard
swift run aok tui
swift run aok settings show
swift run aok settings set engine echo
```

TUI 使用 `aokctl` 和 supervisor control API，不持有内核 root capability。

### PID1 initfs

```sh
(cd runtime && go build -o .build/aok-init ./cmd/aok-init)
(cd runtime && go build -o .build/aok-supervisor ./cmd/aok-supervisor)
make aok-initramfs
```

生成的档案包含 `/init`、`/sbin/aok-supervisor`、`/etc/aok/manifest.yaml` 和
`/var/lib/aok`。

## 仓库结构

| 路径 | 内容 |
| --- | --- |
| `host/` | Swift CLI、TUI 和 VM/control adapter |
| `kernel/` | Linux baseline、AOK patch、配置、kselftest |
| `runtime/` | Go supervisor、engine 协议、provider、sandbox |
| `manifests/` | capability manifest 示例 |
| `docs/` | 架构、ABI、验证和实现边界 |

## 文档索引

| 文档 | 内容 |
| --- | --- |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | 分层、Application 模型、ABI 边界 |
| [docs/IMPLEMENTATION-STATUS.md](docs/IMPLEMENTATION-STATUS.md) | 已冻结决议与当前边界 |
| [docs/PLAN-L0-AGENT-KERNEL.md](docs/PLAN-L0-AGENT-KERNEL.md) | L0 Agent 内核设计计划 |
| [docs/PLAN-AOK-DEEP.md](docs/PLAN-AOK-DEEP.md) | 深度设计决议与调研来源 |
| [docs/abi/](docs/abi/) | fd ABI、engine 协议、control API、application/context 模型 |
| `docs/*-VALIDATION.md` | 各阶段 QEMU 与 runtime 验证证据 |

## 参与贡献

改动项目前请先阅读相关 ABI 与验证文档。运行与影响范围相称的测试，并在
文档中明确区分已实现、已验证和环境阻塞的行为。

## 许可证

AOK 以 [GNU General Public License v3.0](LICENSE) 授权。
