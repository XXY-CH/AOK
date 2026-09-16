# AOK — Agent-native SubOS 架构

## 定位

AOK 是运行在宿主 OS（macOS 27, arm64）之上的真正 SubOS：

- **真隔离**：每个 SubOS 实例 = 一个轻量 Linux microVM（独立内核，基于 Apple Containerization framework）。
- **Agent-native 内核**：内核抽象（进程、调度、上下文、IPC、能力）直接面向 LLM Agent；Linux task、POSIX 和桌面交互都只是实现或外部适配细节。
- **可插拔引擎**：SubAgent 引擎（模型 runtime）只是内核里的一种"进程"，内核不关心里面跑什么模型。
- **Agent 生长环境**：Application 是 Agent 可组合的能力与状态单元，按需求创建、升级、休眠和退役；人类界面只是可选控制面。

与现有方案的区别：E2B/Daytona 有真隔离但只是沙箱服务（无 Agent 抽象）；AIOS/Letta 有 Agent 抽象但是纯用户态 harness（无隔离）。AOK = 两者结合。

## 分层

```
macOS 27 宿主
└─ H  aok-host (Swift, Containerization framework)
   · VM 生命周期 / 资源配额 / pause-resume（VZ 无 snapshot API）
   · VM 后端接口可插拔（当前 Apple Containers；未来 Linux 宿主可加 libkrun/Firecracker）
   · hostfs bridge（virtiofs live mount、artifact import/export）与外部消息 connector
└─ L0  AOK Linux kernel fork + VM 内 PID 1 supervisor
   · aproc/AID、CPU/内存/token 资源域、sched_ext、freezer、capability enforcement
   · AOK syscall 面：Agent-native fd 对象、poll/read/ioctl；POSIX 兼容不属于默认产品 ABI
└─ L1  AOK runtime/libOS（Go，用户态）
   · Agent session、turn、context/KV snapshot、监督策略和协议适配
   · router system agent、web capability stack、message gateway、ACP/MCP/A2A adapter
└─ L2  Agent worker / engine
   · engine trait、echo-engine、claude-engine 和其他 inference backend
   · HostFS/artifact bridge：由 capability 授权的 virtiofs 挂载和 hash artifact 交换
   · 能力模型：manifest → Landlock + seccomp 编译强制；eBPF 审计（后续）
```

Application 不属于某一个 Linux task。它由 Agent 的 durable identity、版本化 contract、记忆与
工具绑定组成，当前由一个或多个 `aproc` incarnation 执行。worker 可以崩溃、冻结、替换或在 VM
重启后重建，而 Application identity、未确认消息和 checkpoint 持续存在。Application 的创建、
生长、迭代和退役由 Agent 需求触发，由 supervisor 和 AOK capability/resource policy 约束。

这里的 `L0/L1/L2` 是目标架构编号。早期文档把 `aok-host` 标为 L0、VM 内 `agentd` 标为
L1；那套编号只保留为迁移历史，不用于新 ABI 或 ACP 边界。ACP 位于 L1 runtime 的对外
adapter，AOK syscall 位于 L0，engine protocol 位于 L1 到 L2 的内部边界。

用户通过 `aok` CLI 与 AOK control API 交互；外部 GUI 复用同一个 control API 和事件流。
CLI/GUI 不直接持有 kernel root capability。ACP 是面向编辑器和外部客户端的协议适配器，
不是 AOK syscall 或内部 IPC ABI。

CLI、GUI 和 ACP 不是 Application 的生存条件。没有 client 连接时，Application 仍可根据
`wake_policy` 继续运行、进入 quiescent，或由 durable mailbox 事件唤醒。

动态模型路由由 L1 router system agent 执行：它根据任务类型、上下文大小、KV 亲和、隐私
capability、延迟和 token 预算选择 `ainf` backend；kernel 只执行 capability、资源域、缓存
兼容性和 usage 对账。路由失败时沿显式的 backend fallback 链降级，不能绕过资源或安全策略。

Web、宿主文件和人类消息都以结构化 Application 接入。Web 默认按 `fetch -> document ->
session -> browser` 逐级升级；宿主文件通过 capability 授权的 virtiofs mount 或 artifact
传输；消息 gateway 把外部 channel 规范化后写入 durable mailbox。三者都不能直接注入
system prompt，也不能绕过 taint、审计和幂等副作用。

## Agent-native ABI 与外部适配器

P0 的内核接口是 AOK fd ABI：对象通过 capability handle 引用，事件通过 `poll/read/ioctl`
传递，上下文通过 `ctx.*` 管理，推理通过 `ainf` 设备提交。AOK 不以启动通用 Linux 用户态、
实现 POSIX 或复刻人类桌面工作流为目标。

旧 Go runtime 和 `/run/aok/syscall.sock` 已从工作树清除。未来如需迁移旧用户态，适配器必须
放在明确命名的 `adapters/legacy/`，并通过 AOK control API 获取受限 capability；它不能成为
PID1、内核 ABI 或 Agent 间 IPC。

能力清单（manifest，YAML）：

```yaml
engine: echo
capabilities:
  fs:
    read:  [/mnt/project/in/**]
    write: [/mnt/project/out/**]
  net: false        # 无网络
  tools: [search]   # 允许的工具白名单
resources:
  mem_mib: 512
  cpus: 1
```

## 目录

- `host/` Swift：未来的 aok CLI 与 VM/control API 适配器
- `kernel/`：Linux AOK fork、patch series、配置片段和 kselftest
- `manifests/` Agent 能力清单示例
- `docs/abi/` AOK fd ABI、control API 和外部 adapter 契约

## 路线

1. 底座打通（aok vm up + Agent runtime handshake）
2. Agent-native 内核原语（aproc、amem、ainf、IPC、event source）
3. 能力模型（Landlock/seccomp/acap）
4. 多开并行调研（MVP 验收）
5. 持久上下文、迁移恢复、eBPF 审计和更多引擎
