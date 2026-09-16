# AOS L0 计划 — Agent-native 内核（AOK 设计）

> 状态：历史设计计划。准备阶段已完成，当前执行状态见 [IMPLEMENTATION-STATUS.md](IMPLEMENTATION-STATUS.md)；本文件回答一个真正的
> Agent-First SubOS 内核（AOK, Agent OS Kernel）应该长什么样、怎么分阶段落地。
>
> **后续**：深度调研已完成，全部悬置决策（语言、安全栈、推理后端、引导路线）已在
> [PLAN-AOK-DEEP.md](PLAN-AOK-DEEP.md) 给出决议，冲突处以那份文档为准。

---

## 1. 诊断：agentd 差在哪

现有 Phase 1–2 的实现里，agentd 是 **Linux 容器内的一个守护进程**。它借用了 Linux 的
全部底层抽象（Unix 进程、goroutine、Unix socket、ext4 文件），agent 语义只是架在这些
抽象之上的一层薄 RPC。具体缺陷：

| 内核职责 | 现状（agentd） | 问题 |
|---|---|---|
| 进程抽象 | Agent = goroutine + 结构体 | 没有身份、没有生命周期语义、与 Linux 进程混淆 |
| 调度 | Go runtime 调度 goroutine | 调度的是协程不是 Agent；无优先级、无抢占、无配额 |
| 内存管理 | 无 | 上下文窗口完全由引擎自治；无分页、无 fork、无回收 |
| 设备模型 | 无 | LLM 推理藏在引擎里；无 token 计量、无 cache 亲和 |
| 安全 | 无 | manifest 只是 JSON 字段，没有任何强制点 |
| IPC | 无 | 只有共享目录 |
| 可观测 | 容器 stdout | 无进程级审计 |

结论：要"Agent 融入 OS L0"，内核自身的抽象必须重造为 Agent 形状，Linux 降级为
硬件抽象层（HAL）。

## 2. 分层重定义

```
H   机器层   macOS 宿主 + Apple Containerization microVM
            （VM = SubOS 的"裸机"；virtiofs/vsock = 设备总线）
L0  内核层   AOK — Agent 原生内核（Linux fork；策略与协议仍有用户态部分）
            · aproc   Agent 进程（一等调度实体，带身份 AID）
            · 调度器   turn 级抢占调度 + token 公平配额
            · amem    上下文虚拟内存（KV=页帧，消息=页，记忆=文件系统）
            · ainf    推理设备（/dev/ai0，fd 化会话，后端可插拔）
            · acap    对象能力安全（启动即 capability mode）
            · 端口/信箱 IPC + Plan 9 命名空间 + 监督树（PID1 = supervisor agent）
L1  libOS 层 每个 Agent 的引擎运行时（exokernel 教训：harness=每进程专属库态）
L2  用户层   系统 Agent（shell=ash、fsd 存储代理、auditd）+ 用户 Agent
```

本计划采用的目标分层与现有文档中的旧编号不同。旧架构里的 `aos-host=L0`、VM 内
`agentd=L1`、引擎 `L2` 保留为迁移参考；目标架构把 Linux kernel 扩展和 AOK kernel
objects 视为 L0，AOK runtime/libOS 视为用户态控制层。`aos-host` 仍负责 VM 生命周期，
但不持有 Agent 的 root capability。

这与业界两个失败阵营的区别：**隐喻 OS**（AIOS/OpenFang/Letta：有 agent 语义、无隔离、
无内核强制点）和**真隔离设施**（microsandbox/e2b/K8s agent-sandbox：有隔离、把 agent
当不可信代码、无 agent 语义）。AOK 的定位 = 交集："agent=进程 + 强隔离 + 记忆/权限/资源
治理"，这个交集目前市场上是空位。

## 3. AOK 的内核子系统与对象（设计）

### 3.1 aproc — Agent 进程

```
aproc := {
  AID        内核全局唯一身份（可传 capability）
  ctx_as     上下文地址空间（见 3.3）
  caps       能力表（不可伪造的内核对象引用，可收窄/传递/撤销）
  fdt        工具/设备 fd 表（每个 fd = 内核对象引用 + 权利位）
  mailbox    端口（IPC 收件箱）
  engine     引擎绑定（libOS 适配器，见 3.7）
  budget     资源账户（token/调用次数/墙钟，cgroup 式层级继承）
}
```

Agent **不是** Unix 进程。Unix 进程只是 aproc 可以持有的一种"工作者句柄"（和工具 fd
同级）。aproc 是当前 boot 的执行化身，生命周期为 spawn → runnable → running → frozen
（可冻结可恢复，替代 OOM-kill）→ exited/failed → reaped。task 或 aproc 的回收不等于
Agent 或 Application 终止；持久身份、上下文、durable mailbox 和关系由 runtime/LSFS
保存，并可在新的 generation/AID 上恢复。Application 的生长、迭代、休眠和退役契约见
[AOK Application Model](abi/application-model.md)。

### 3.2 调度器 — turn 级抢占 + token 公平

- **调度单位是 turn**（一次 LLM 推理或一次工具执行），不是线程。
- **公平**：VTC（Virtual Token Counter, OSDI'24）——每 aproc 一个计数器，输入 token
  记 1、输出 token 加权记账，总是服务计数器最小者。这是"token 时间片"的直接算法。
- **优先级**：MLFQ（FastServe 的 skip-join 多级反馈队列），高优先级 agent 可抢占低
  优先级的排队请求。
- **抢占点**：turn 边界。抢占 = freeze + 上下文快照（AIOS 已证明 logits/文本级快照
  可恢复）；换出的 KV 按 **TTL 保留**（Continuum 的核心机制），恢复时前缀命中则免重算。
- **配额**：budget 层级 = SubOS → 用户 → agent → turn（cgroup v2 式继承）；超限渐进
  降级：节流 → freeze → 通知监督者，**永不 OOM-kill**。
- **双向协议**（AgentCgroup 首创）：agent 可经 syscall 向调度器申报 hint（"我这步很急/
  可延迟"），调度器可向 agent 注入结构化反馈（"配额将尽，请收敛"）。

### 3.3 amem — 上下文虚拟内存

三层统一建模（业界已收敛的三层映射）：

| 层 | 物理对应 | 管理者 |
|---|---|---|
| KV cache | 页帧（PagedAttention 式 block） | ainf 设备后端 + 内核页表 |
| 消息上下文 | 进程地址空间（页 = 消息段/记忆块） | amem |
| 长期记忆 | 文件系统（LSFS：日志结构 + 语义检索 + 版本回滚） | fsd 存储 Agent |

- **页表**：aproc 的 ctx_as 把"上下文窗口"映射到消息页与记忆块；page fault = 上下文
  未命中，内核从 LSFS/共享块换入并记录。
- **fork = COW**：fan-out 时子 agent 共享父上下文前缀（prompt-cache 命中天然是 COW 的
  物理实现，HumanLayer context forking 的内核化）；写时才复制消息页。
- **淘汰必须上下文感知**：不能纯 LRU（vLLM RFC：10% 的 block 承载 77% agentic 流量），
  按 KVFlow 思路以 agent 工作流位置决定保留优先级。
- **记忆治理**（MemOS MemCube 抽象 + Recall 教训）：每条记忆带来源/权限/生命周期元数据；
  采集默认 opt-in；支持整体遗忘。

### 3.4 ainf — 推理设备

LLM 推理是 SubOS 的第一等**设备**，不是引擎的私有实现。fd 化接口：

```
open("/dev/ai0", {model, cap})      # 会话句柄（需推理能力 cap）
write(session, prompt_page)         # 提交 turn
read(session)  -> token 流          # completion 事件流
ioctl(session, PRIORITY|KV_TTL|ABORT|HINT)
```

- **后端可插拔**（vAccel 的 virtio 插件 ABI 是现成骨架）：
  - `backend-metal`：宿主 llama.cpp（Metal），经 vsock 桥接（VM 内 AF_VSOCK → 宿主 UDS）；
  - `backend-cloud`：Claude/OpenAI API（远程设备）；
  - `backend-afm`：宿主 Apple Foundation Models daemon（已验证：普通 CLI 可调用，推理
    发生在系统服务 IntelligencePlatformComputeService 内；仅 4096 token 上下文，只用于
    轻量路由/摘要/裁决）。
- **内核中介**：每次 infer 过 acap 检查 → budget 记账（usage token 由后端上报）→ 调度
  （VTC+MLFQ）→ cache 亲和放置（SGLang 证明前缀亲和是有效调度权重）。
- **抢占语义下沉**：vLLM 的 recompute/swap 两档抢占、llama.cpp 的 slot KV save/restore、
  流式 abort，统一为设备的 ioctl 语义。现状没有任何 runtime 完整暴露这些，这是 ainf
  要补的设备抽象层。

### 3.5 acap — 对象能力安全

- **启动即 capability mode**（Capsicum）：agent 无 ambient authority——没有隐式文件系统/
  网络/环境变量权威，一切权威来自 capability。
- **capability = 不可伪造的内核对象引用**，可收窄（`web.fetch(anything)` →
  `web.fetch(url=cap)`）、可传递（fd passing）、可撤销。模型：seL4 语义 + E 语言分布式
  语义；CaMeL 已证明这是 prompt injection 的结构性防御。
- **污点跟踪**：数据流携带来源/触发器元数据；策略（manifest 编译产物）在每次工具调用
  时由内核检查"污点数据不得流向外发类工具"。
- **危险原语物理缺席**：从 syscall 面剔除不可回滚的操作（Windows 文件 connector 无
  delete 原语的思路）；确需不可逆操作 → 慢路径，需人类确认（AIOS ask_permission）。
- **审计**：每次 capability 授予/使用/撤销入 Merkle 链（OpenFang 方向），可验证不可篡改。

### 3.6 IPC + 命名空间 + 监督

- **IPC 原语**：port（Erlang mailbox，点对点）+ topic（pubsub）+ fd passing（能力传递）。
  **MCP 和 A2A 是传输驱动不是 IPC 本身**：MCP=本地/远端 RPC 驱动，A2A=服务发现驱动——
  两者都只解决语法，授权/配额/身份/生命周期由 AOK 补齐（这正是微软在 Windows 外围补、
  Docker 用 gateway 补的同一块，AOK 把它放进内核）。
- **命名空间（Plan 9）**：每 agent 私有命名空间；工具/记忆/其他 agent 都呈现为文件树
  （`/tool/web`、`/mem/core`、`/proc/<AID>/…`）；工具调用 = 经能力门控挂载后的文件操作；
  agent 自身状态（上下文页表、配额、调度统计）暴露为 /proc 式文件，`cat` 即观测。
- **监督树（BEAM）**：PID1 是 supervisor agent；子 agent 崩溃按策略重启
  （one_for_one/rest_for_one + 退避），重启利用 KV TTL 热恢复前缀、只重建消息页。
  故障隔离在子树内——"let it crash" 的 agent 版。

### 3.7 L1 libOS（exokernel 教训）

内核只做多路复用与保护；提示组装、记忆格式、工具 schema 这些 agent harness 下沉为
**每 agent 的 libOS**（engine 适配器）。现有 engine trait 即 libOS 的雏形，Claude 引擎、
本地模型引擎、乃至"ReAct/AutoGen 兼容适配器"（AIOS 的框架重定向思路）都是 libOS 实例。

## 4. 依赖的现实约束（调研已核实）

- KV cache 控制权：云 API 不暴露 KV 层 → amem 必须分级降级（可控 KV 的后端做页帧级
  管理；纯 API 后端退化为消息级分页，MemGPT 已证明可行）。
- Apple FM 4096 token 上下文 → 只当轻量设备后端（路由/摘要/裁决），不做主力推理。
- microVM 内 vsock：Apple Containerization 支持性与 virtiofs 一样是基础前提，Phase P2
  第一件事就是打通 vsock 桥（不通过则退化为宿主侧 TCP 端口转发）。
- agent 负载的 OS 层特征（AgentCgroup 实测：工具调用占端到端延迟 56–74%、内存峰均比
  15.4x）→ 冻结/节流优于杀，工具调用边界是配额分层的正确位置。

## 5. 实施计划（分阶段，当前不动代码）

**P0 — ABI 与 Linux fork 章程冻结（纯文档，1 个迭代）**
产出 `docs/abi/`：aproc/AID/capability fd syscall 表（对象/方法/错误码）、CPU/内存/token
资源记账、capability 策略语言文法、amem 页表格式、ainf 设备 ABI、AID/port 命名规范，
以及 ACP v1 profile、control API 和事件流契约。评审门：能据此独立实现两个互操作实现，
并互操作完成同一组 turn、事件、工具产物和恢复测试。现有 agentd 的 `spawn/wait/signal/ps`
只能作为可选外部迁移适配器，不直接充当 ACP wire ABI。

**P1 — 内核骨架与资源域（aproc + 调度 + amem v1 + port IPC + /proc）**
AOK 以 PID 1 启动于 SubOS VM；kernel 创建不可变 AID 和 root capability，并把 root
capability 交给 PID1 supervisor。`aproc` 通过新增 syscall 返回 anon inode fd，可绑定一个或
多个 Linux task；首个 task 由 aproc 创建，子 task 默认继承，跨 aproc attach 必须有
capability。Linux task 只是执行载体，通用 Linux 用户态由外部 adapter 提供。CPU 调度使用 `sched_ext`，turn 边界由 runtime
协作让出。验收：CPU/内存资源域可观测，fan-out 3 个 COW fork 的 aproc，稳定上下文前缀不复制，工具大结果只传 artifact handle，调度统计可通过
`/proc` 读取，freeze/恢复闭环，监督树可重启崩溃 agent 且上下文只重建消息页。

**P2 — ainf 推理设备（vsock 桥 + 三后端 + token 记账）**
验收：同一 fd ABI 下切换 metal/cloud/afm 三后端；token budget 超限触发渐进降级（节流→
freeze）；前缀亲和放置生效（同源 fan-out 的 KV 命中率可测量）。

**P3 — acap 强制（capability + 污点 + 审计链）**
权限请求由 AOK permission broker 创建并关联 `request_id/aid/turn_id/tool/scope/ttl`。
CLI 或外部 GUI 提供确认界面；ACP adapter 将同一请求映射为
`session/request_permission`。用户批准只是输入，kernel 仍检查父 capability、manifest、
污点和资源策略，最后只授予可收窄、可撤销、带审计的 capability。验收：无 cap 的 fs 外发
被 kernel 拒绝并留 Merkle 审计；污点数据流向外发工具被拦截；不可逆操作走确认慢路径。

**P4 — 系统 Agent 与多协议驱动（supervisor/fsd/ash + ACP/MCP/A2A）**
CLI 是第一用户入口，外部 GUI 复用同一个 control API 和事件流；ACP 是面向编辑器或外部
客户端的 session adapter，MCP 是工具驱动，A2A 是 Agent 间驱动。ACP v1 adapter 至少
实现 `initialize`、`session/new`、`session/prompt`、`session/update`、`session/cancel`
和 `session/request_permission`，并把 session、turn、mailbox、permission 映射到 AOK
对象。验收：shell 即 agent（ash 在命名空间里用文件操作完成 spawn/inspect）；外部 MCP
server 以驱动身份接入且每 server 独立能力域；真实 ACP client 可完成多轮 prompt、流式
update、cancel 和权限拒绝。

**P5 — MVP 验收：多开并行调研**
一条命令：`ash` 内 spawn 1 个 planner agent → COW fork N 个 researcher → 经 ainf 三后端
并行推理与工具调用 → port 回传 → aggregator 汇总写 LSFS → 全程 /proc 可观测 + 审计链
完整。这是"Agent 融入 L0"的端到端证明。

**P6+ — 远期**：eBPF 级观测桥、microVM 快照迁移（Llumnix 式 agent 迁移）、跨 VM A2A
网格、AOK 核心是否迁 Rust（见决策点）。

## 6. 已冻结的方向

1. **目标**：研究与产品结合。AOK 是完整的 Agent-native 深度 fork；Linux/POSIX 用户态、
   GUI、CLI 和 ACP 都是外部可选连接器，现有 Go `agentd` 降为迁移期 runtime/reference
   implementation，不能定义内核 ABI。
2. **内核基线**：选择上游 LTS，维护 out-of-tree patch series 和独立 `kselftest`；先在
   QEMU arm64 验证，再接入 Apple microVM。
3. **aproc 与 ABI**：新增独立 `aproc` kernel object，一个 aproc 可管理多个 Linux task，
   通过 anon inode fd 暴露；首个 task 由 aproc 创建，子 task 默认继承，跨 aproc attach 必须
   持有 capability。事件使用 `poll/read/ioctl`。
4. **身份与能力**：AID 由 kernel 生成且不可变；kernel 启动时创建 root capability，仅交给
   PID1 supervisor，其余 capability 只能收窄、传递和撤销。CLI、GUI 和 Agent 均不得持有 root
   capability。
5. **资源域**：首版只覆盖 CPU、内存、token。token 采用预留配额、后端 usage 回报和 kernel
   对账；无法回报时按保守上限计费。超限统一执行 `throttle -> freeze -> supervisor event`，
   OOM kill 默认关闭。
6. **调度与冻结**：CPU 调度使用 `sched_ext`，turn 边界由 AOK runtime 协作让出。kernel
   freezer 只停止 task；runtime 负责 turn、KV、context 快照，并显式报告恢复级别。
7. **用户入口**：`aok` CLI 是第一入口，外部 GUI 复用 control API 和事件流。ACP 是
   client-facing session adapter，不是 syscall 或内部 IPC ABI；MCP 是工具驱动，A2A 是
   Agent 间驱动。

## 7. 用户交互契约

用户通过 `aok` CLI 创建和管理 session/aproc，执行 inspect、cancel、freeze、resume 和
资源查询。外部 GUI 使用同一 control API，不绕过 AOK 对象模型。

交互路径为：

```
CLI / External GUI
        |
        v
AOK Control API + Event Stream
        |
        v
Permission Broker / PID1 Supervisor
        |
        v
Linux AOK kernel objects and resource domains
```

用户身份由 host 登录身份映射为 AOK user principal，再由 supervisor 委派受限 capability。
权限请求至少关联 `request_id`、`aid`、`turn_id`、操作、scope 和 TTL。CLI/GUI 负责呈现和提交
确认，kernel 仍检查 manifest、父 capability、污点和资源策略，并记录最终授予或拒绝。

用户交互不是 Application 的运行时依赖。Agent 可以通过 channel、durable mailbox、memory
和 event 自主创建或调整 Application；CLI、GUI、ACP 只提供观察、批准和调试。Application
没有 client 连接时，仍按 `wake_policy` 继续工作、进入 quiescent 或等待事件唤醒。

## 8. P0 必须补齐的规格

- AID、aproc fd、Linux PID/pidfd、ACP `sessionId`、`turnId` 的作用域和生命周期；
- AOK syscall 错误码、事件顺序、背压、断线重放和取消语义；
- CPU/内存/token 记账、pressure 阈值和 `throttle -> freeze` 状态机；
- capability 表示、收窄、传递、撤销和审计记录格式；
- ACP v1 profile（固定版本/schema commit）与 control API 的边界；
- event source ABI：timer、port、LSFS 变更的绑定、唤醒、ack、overflow 和 replay；
- context ABI：stable prefix/mutable tail、hot/warm/cold、KV affinity、COW、pagein/evict、
  artifact handle、compaction manifest、provenance、recovery level 和效率指标；
- Linux fork 的 patch 分层、配置开关、QEMU arm64 启动方式和 `kselftest` 验收矩阵。
