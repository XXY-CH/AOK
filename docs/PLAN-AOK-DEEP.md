# AOK 深度设计决议（调研综合版）

> 状态：设计决议；准备阶段已落地，AOK kernel patch 尚未开始。本文是 [PLAN-L0-AGENT-KERNEL.md](PLAN-L0-AGENT-KERNEL.md) 的续篇：
> 六路深度调研（底座可行性、内核蓝本、安全栈、缓存物理、记忆存储、引擎协议）已完成，
> 每个悬置决策在此给出**决议 + 依据**。各节末尾附关键来源。

---

## 0. 调研发现的总览（一句话版）

1. **底座**：vsock 是 containerization 的主干通道（库 API 现成、guest 内核内建），自定义 init 有三条成熟路线；VZ 无 snapshot API，但 pause/resume 可用。
2. **内核蓝本**：ABI 抄 Zircon（handle/rights/channel/job），内部组织抄 gVisor（task goroutine 状态机、引用计数、锁序文档），监督抄 OTP。
3. **安全栈**：Biscuit（令牌）+ Cedar（对象策略）+ 内核级污点层（CaMeL 语义）+ Landlock ABI6/seccomp/unotify（Sandlock 分裂模型）+ Sigsum 式审计链。
4. **缓存物理**：五个推理后端的"页帧"形态各异且都有精确 API（断点/TTL/租金/命中字段），统一抽象可落地。
5. **记忆存储**：LSFS = git 语义 + modernc.org/sqlite 单库（WAL+FTS5+内置 vec）+ MemOS 式治理元数据，纯 Go 可行。
6. **引擎协议**：JSON-RPC over UDS v1（借鉴 ACP 流式语义 + containerd 生命周期语义），预留 proto→ttrpc→Cap'n Proto 演进；ACP 本身是独立的 client-facing adapter。
7. **Application 以 Agent 为中心**：Application 是持久的能力、状态和关系单元；按 Agent 需求生长、迭代、休眠和退役，执行化身可替换，用户界面不构成生存条件。
8. **事件源是一等 ABI**：timer、port 和 LSFS 变更可将 dormant/frozen Application 标记为 runnable；事件绑定 `application_id`，具备顺序号、ack、去重、overflow 和 replay 语义。

---

## 1. 决议 D1：底座与 SubOS 引导路线

**发现**（源码级查证）：
- 每个 VZ VM 硬编码挂载 `VZVirtioSocketDeviceConfiguration`；库层公开 `dial(port)/listen(port)` API；guest 内核 `CONFIG_VSOCKETS=y` 内建。vsock 历史上有并发 relay bug（#712/#713 已修，#911/#2247 部分未闭）。
- VM 的 PID 1 恒为 vminitd（`init=/sbin/vminitd` 硬编码）；三条自定义路线：(a) `--init-image` wrapper 先跑 AOK 逻辑再 exec 真 vminitd；(b) 顶替 vminitd 复刻其 vsock gRPC 协议（SandboxContext.proto V3）；(c) 完全自制 initfs + 自有 vsock 协议，只用 VM 层 API。
- 控制面：`VMConfiguration`（CPU/内存/virtiofs/网络/串口）+ `VZInstanceExtension` 可注入任意 VZ 设备；pause/resume 可用（会连 vsock 一起暂停）；**无 snapshot**。
- 实测开销：~0.9s/VM 启动；2–3 VM 共 ~1.2GB 宿主进程内存；默认 1GiB/VM 可 overcommit。
- 风险：macOS 27 beta 的 TCC 本地网络权限曾致端口转发失效（#2029）；vminitd 长驻并发挂起（#2247/#2258）。

**决议**：
- **宿主↔VM 控制面 = vsock**（避开 1024 端口，用高位段），应用层加心跳与重连（vsock relay 历史 bug 的对冲）。TCC/网络类风险随 vsock 天然规避。
- **引导路线：P1 用 (a)（wrapper init，保住全部生态快速迭代），P2 迁到 (c)（自制 initfs，AOK 成为真 SubOS 的 PID 1）**。最终形态不走 vminitd，从根上隔离 #2247/#2258 类问题。
- 版本锁定：SwiftPM 依赖 `containerization .upToNextMinor(from: "0.45.0")`；AOK fork 基线锁定为上游 stable `linux-6.18.y`。Apple Containerization 调研中的 `6.14.9+` 只作为 guest 兼容性验证下限，不能替代 fork 基线。
- 快照策略：不押注 VZ；agent 级 checkpoint = amem 状态 + LSFS commit（Linux 侧），VM 级迁移 = freeze agents → vsock 同步状态 → 目标 VM 恢复（P6）。

来源：[VZVirtualMachineInstance.swift](https://github.com/apple/containerization/blob/main/Sources/Containerization/VZVirtualMachineInstance.swift)、[runtime-configuration.md](https://github.com/apple/container/blob/main/docs/runtime-configuration.md)、[issue #2029](https://github.com/apple/container/issues/2029)、[RepoFlow 基准](https://www.repoflow.io/blog/benchmarking-apple-containers-vs-docker-desktop)。

## 2. 决议 D2：内核对象模型（抄 Zircon）与内部组织（抄 gVisor）

**对象模型 = Zircon 语义**：
- 对象类型集（AOK v1）：`Job`、`Aproc`（Agent 进程）、`Channel`（mailbox）、`ToolHandle`、`Session`（推理会话）、`Vmo`（共享内存块）、`EventPair`。对象有永不复用的 64 位 koid。
- **权限在 handle 上不在对象上**；rights 位集照抄：`BASIC = DUPLICATE|TRANSFER|WAIT|INSPECT` + `READ|WRITE|EXECUTE|DESTROY|ENUMERATE|SET_POLICY`。duplicate/replace 只能单调缩减；转发时可剥离 DUPLICATE（"锻造不可再复制能力"——delegation 必抄）。
- Channel 携带 handle = 能力传递；datagram 语义、全有或全无消费、端点关闭销毁在途消息、**PEER_CLOSED 保证**、txid call 语义。
- aproc = Job（策略/配额/监督边界）+ 内含执行体；**创建即授权**（无 job handle 不能 spawn 子 agent）；job policy：`BAD_HANDLE=KILL`、`NEW_*=DENY`；异常沿 job 树上冒，策略/配额向下继承。
- supervisor agent = 用户态策略对象（OTP intensity/max_restarts），内核 job policy = 硬化保底；`critical` 标记等价 `zx_job_set_critical`。

**内部组织 = gVisor 工程模式（Go）**：
- 每个 aproc 一个 task goroutine + **显式状态机 run loop**（runnable/running/frozen/exited，状态机替代栈切换）；freeze = 状态物化 + 伪中断（gVisor checkpoint 模式：被中断的推理可经快照恢复）。
- `pkg/refs` 式引用计数（TryIncRef 弱引用、0 计数 IncRef 即 panic、leak 三档检测）；Context 代表 goroutine 而非 operation；锁序注释即架构文档。
- 系统服务拓扑：**fsd = 独立被监督进程**（gofer 模式：FD 化 API、不信客户端、lisafs 式 flipcall 快路径）；**ainf = 内核控制对象 + 用户态被监督 backend**（kernel 负责 fd、配额、能力和事件，backend 负责模型数据面）；**引擎 libOS = 独立进程**（见 D7）。

来源：[Zircon handles/rights/jobs/channel](https://fuchsia.dev/fuchsia-src/concepts/kernel/handles)、[gVisor task_run.go](https://github.com/google/gvisor/blob/master/pkg/sentry/kernel/task_run.go)、[pkg/refs](https://github.com/google/gvisor/blob/master/pkg/refs/README.md)、[lisafs](https://github.com/google/gvisor/blob/master/pkg/lisafs/README.md)。

## 3. 决议 D3：调度器（VTC + MLFQ + freeze + 双向 hint）

按 PLAN-L0 §3.2 落实，补充精确机制：
- VTC 计数器挂在 aproc 的 budget 对象上；输入 token 记 1、输出按模型单价加权（云后端成本可直接折算）。准入 = 计数器比较；超限 = 节流（拉长 turn 间隔）→ freeze → 通知监督者。
- **抢占 = turn 边界 freeze**：LLM 推理一经提交不可中断（云 API 无此语义），抢占点在 turn 之间与工具执行间隙；被抢占 agent 的 KV 按 TTL 保留（云后端 = 命中续租，见 D5），恢复时前缀命中免重算。
- hint 协议：syscall `sched.hint(priority_urgency)`；内核反馈走 agent mailbox（结构化消息：`budget_low`/`preempted`/`evicted`）。

来源：[VTC/OSDI'24](https://www.usenix.org/system/files/osdi24-sheng.pdf)、[FastServe](https://arxiv.org/abs/2305.05920)、[Continuum](https://arxiv.org/abs/2511.02230)、[AgentCgroup](https://arxiv.org/html/2602.09345v2)。

## 4. 决议 D4：amem 三层页表与 fork

- 页帧层按后端分"物理"（见 D5）；消息页 = 内存中的消息段/记忆块；磁盘层 = LSFS（D6）。
- **fork = COW**：fan-out 时子 aproc 的 ctx_as 初始页表 = 父的只读映射；云后端的物理实现 = 共享前缀 + 各自 cache 断点；本地后端 = llama.cpp `cache_prompt` 前缀复用（llama-server 响应 `tokens_cached` 可测量命中率）。
- 淘汰：上下文感知（KVFlow 思路）——按 aproc 工作流位置保留；LRU 仅作兜底。
- 记忆治理元数据（MemOS MemCube 字段）：`origin_signature`、`access_control`、`lifespan_policy`、`version_chain`、`access_patterns`——直接进 LSFS schema（D6）。

## 5. 决议 D5：ainf 推理设备与"页帧物理表"

设备 ABI（fd 语义）不变（PLAN-L0 §3.4），补充后端物理映射——这是本次调研最重要的落地成果之一：

| 后端 | 页帧 | TTL/保留 | fork 表达 | 抢占代价 | 命中字段 | 计费 |
|---|---|---|---|---|---|---|
| Anthropic | ≤4 个 cache_control 断点的前缀快照 | 5m 命中免费续租 / 1h（写价 2×） | 共享 system/tools 前缀即共享页 | 前缀变 = 全量重写 | `cache_read/creation_input_tokens` | 读 0.1×，写 1.25×/2× |
| OpenAI | 机器本地渲染后 KV（隐式 1024+128n） | in_memory ~5–10min 或 24h retention；5.6+ 显式断点 30m | `prompt_cache_key` 亲和路由 | 前缀变 = miss | `cached_tokens` | 旧代无写费；5.6+ 读 0.1×写 1.25× |
| Gemini | `CachedContent` 具名对象（唯一可寻址） | TTL 可改期续租（唯一支持显式续租） | 缓存名传入即 fork | 删除即重建 | `cached_content_token_count` | 读 10% + 租金 $1–4.5/M tok/hr |
| llama.cpp | slot 内连续 KV（`-c` 均分） | 常驻；save/restore 可换出磁盘 | `cache_prompt` + `--cache-reuse` | 覆盖即重算 | `tokens_cached`/`GET /slots` | 免费（非确定性风险） |
| vLLM | 引用计数满 block（hash 链） | LRU 至 ref_cnt=0 | 同哈希链跨请求共享 | recompute（前缀块保留） | `gpu_prefix_cache_hit_rate` | 免费 |
| mlx-lm | LRU 段缓存（system/user/assistant 边界） | 字节预算内 LRU | 段前缀匹配 | OOM 风险史 | `prompt_cache_count` | 免费 |

统一接口：`frame(fingerprint) → {ttl, rent, hit_field}`；云后端 TTL 续租/驱逐是尽力而为 hint，本地后端承诺强保留。
- **首选后端（P2）= llama.cpp/Metal**：唯一有完整 save/restore 换页原语（`POST /slots/{id}?action=save|restore`），能真实验证页帧管理；Anthropic 后端并行接入（用 `max_tokens:0` 预热 + 5m/1h 断点策略）；AFM 只做轻量路由/摘要设备。
- token 记账：各后端 usage 字段 → budget 扣费；云后端把 cache 写价/租金计入 VTC 权重。

来源：[Anthropic caching](https://platform.claude.com/docs/en/docs/build-with-claude/prompt-caching)、[OpenAI caching](https://developers.openai.com/api/docs/guides/prompt-caching)、[Gemini explicit caching](https://ai.google.dev/gemini-api/docs/generate-content/caching)、[llama-server README](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)、[vLLM APC](https://docs.vllm.ai/en/latest/features/automatic_prefix_caching.html)。

## 6. 决议 D6：LSFS v1 = git 语义 × SQLite 单库 × MemOS 元数据

- **物理存储：modernc.org/sqlite（纯 Go，v1.47.0 起内建 sqlite-vec 与 FTS5）单文件库**，`CGO_ENABLED=0` 交叉编译成立。这是唯一同时满足"嵌入内核 + MVCC 快照 + 词法+向量检索 + 崩溃一致"的 Go 方案（LanceDB 无 Go SDK、DuckDB VSS 持久化风险、纯 CAS 双写一致不可控）。
- 对象模型（git 语义进 schema）：`objects(sha256, kind∈{blob,tree,commit,entry_meta}, payload)` + `refs` + `commits`；写入事务 = 新对象+新 commit+ref 前移+head 索引更新，**一个 BEGIN IMMEDIATE 全或无**；WAL 重放兜底崩溃。
- 索引层：`entries_head` 物化当前视图，FTS5（BM25）+ vec0 KNN → RRF(k=60) 融合 → ACL/visibility/TTL 过滤。索引与对象同事务，无双写一致问题。
- 治理：entry_meta 带 MemOS 字段；软遗忘（过滤器排除）→ 硬遗忘（tombstone commit）→ 可达性 GC（git gc 语义）→ 冷归档（vault）。
- 多 agent 并发：单写者 + ref 分支（每 agent 一个 ref 命名空间，写完 merge——Letta worktree 语义的内核化）；读 = WAL 快照 + 查询时 ACL 过滤。
- **自实现 blob/tree/commit 编解码（zlib+sha256，几百行）**——不依赖 go-git（非线程安全、memfs 脏 index 前科）。

来源：[modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)、[SQLite WAL](https://www.sqlite.org/wal.html)、[Letta Context Repositories](https://www.letta.com/blog/context-repositories/)、[MemOS](https://arxiv.org/abs/2507.03724)、[Dolt Prolly Tree](https://www.dolthub.com/blog/2024-12-19-merkle-trees-for-file-systems/)。

## 7. 决议 D7：引擎协议 v1 = JSON-RPC over UDS + containerd/ACP 语义

- **引擎 = 独立长驻进程**（崩溃隔离），协议 v1 用 newline-delimited JSON-RPC 2.0 over UDS——引擎是异构第三方（Node/Python/Go），JSON-RPC 是唯一"任何语言 100 行写对端"的选项；token 流 KB/s 级，性能无感；`socat/jq` 可直调。
- 生命周期抄 containerd shim v2：约定式发现（`aok-engine-<name>` 二进制）、握手回 `{"aok_version":1,"address":"unix://…","capabilities":[…]}`、少量 MUST 方法 + `ErrNotImplemented`、**事件有序性 MUST**（`session/started → chunk* → done|failed`）、引擎自带协议外 `cleanup` 子命令（崩溃恢复路径）。
- 流式/取消抄 ACP + vLLM：`session/prompt` 单请求 + `session/event` 通知流；弱取消 = cancel notification，强取消 = `session/abort(id)` 幂等方法；背压不用流控，用 `session/suspend|resume` 显式原语。
- capability 表达：内核对象引用以连接作用域的 handle ID 随 JSON 传（`{"$acap":"sess-42/term-7"}`）；OS fd 通过 SCM_RIGHTS 侧信道传递。路径只能作为受策略约束的数据标识，不能作为 capability 或权限证明。
- 演进路径：v1.5 proto 化（JSON 结构 ↔ proto 一一对应）→ v2 ttrpc 帧（多路复用成为真需求时）→ 远期 acap 原生化（Cap'n Proto，capability 一等公民）。
- 安全：引擎 UDS 权限是协议一部分（CVE-2020-15257 教训）。

来源：[containerd runtime-v2](https://github.com/containerd/containerd/blob/main/docs/runtime-v2.md)、[ttrpc](https://github.com/containerd/ttrpc/blob/main/PROTOCOL.md)、[ACP prompt-turn](https://agentclientprotocol.com/protocol/v1/prompt-turn.md)、[Cap'n Proto RPC](https://capnproto.org/rpc.html)。

## 8. 决议 D8：acap 安全栈与语言拓扑（解开 Go/Rust 矛盾）

安全栈调研的结论（Biscuit + Cedar 原生 Rust 库最强）与内核 Go 化存在张力，**用进程拓扑解开**：

```
AOK Linux kernel（C/Rust）──▶ AOK runtime/libOS（Go）──每次工具调用──▶ acapd（Rust 被监督进程）
                                                                  · Biscuit 令牌校验（Ed25519 块链）
                                                                  · Cedar 对象策略（cedar-policy crate）
                                                                  · CaMeL 式污点元数据递归检查
                                                                  ◀── allow / deny / human_confirm ──
```
- 这正是 Sandlock 的 unotify supervisor 模式（动态决策路由到用户态 supervisor）：强制点唯一（所有工具调用都过内核 syscall 面），acapd 逻辑上是内核的"策略模块"，物理上是进程，UDS 往返 µs 级，淹没在 LLM 延迟里。
- 令牌：**Biscuit**（Datalog check 块 = 可收窄权限、seal = 不可再委托、revocation_id + 短 TTL 双撤销）；UCAN 仅作对外互操作边界。
- 策略：**Cedar** 管"哪个 agent 可对哪个对象引用做什么"（形式化验证、毫秒有界延迟、cedar-for-agents 是 AWS 的 agent 授权方向）；**污点层自研**（Cedar/Rego 都表达不了数据流；CaMeL 实证 949 注入 0 成功）。两级决策任一拒绝则拒；污点失败默认降级为人工确认（防用户疲劳）。
- 身份：每 agent ed25519 密钥对（内核托管私钥），SPIFFE ID 命名 + did:key 序列化；Biscuit rootKey 对齐。
- OS 强制（VM 内防御纵深）：**Landlock ABI6（Linux 6.12+，containerization 内核 6.14.9+ 满足）+ seccomp-BPF + unotify 慢路径**；策略编译 = nono 式单一 CapabilitySet → 一次 restrict_self + 一个过滤器；**默认阻断 Unix domain socket**（GHSA-27vp 逃逸前车之鉴）；Landlock domain 不可变 → 撤销 = 内核能力表软操作 + 进程硬终止两级。
- 审计：**Sigsum 式 Merkle 透明日志**——事件 = hash(keyId, action, objectId, taint, decision, time)，STH + witness 联签（本机独立进程 + 宿主侧 + 可选远程），外部锚点防篡改。

来源：[biscuitsec.org](https://www.biscuitsec.org/)、[Cedar](https://cedarpolicy.com/)、[CaMeL](https://arxiv.org/abs/2503.18813)、[Sandlock](https://arxiv.org/html/2605.26298v1)、[nono](https://github.com/nolabs-ai/nono)、[Landlock ABI](https://docs.kernel.org/userspace-api/landlock.html)、[Sigsum](https://www.sigsum.org/)。

## 9. 决议 D9：实现语言（终局）

- **Linux kernel fork 保持上游语言边界**：AOK 内核对象、调度、freezer、LSM 和 syscall patch 用 C（必要处使用上游允许的 Rust），不能把 Go runtime 当作 Linux kernel 实现。Go 用于用户态 AOK runtime/libOS、fsd 和协议实现；现有 `kernel/` Go 代码是迁移期 reference implementation。
- **acapd 用 Rust**（Biscuit/Cedar 参考实现同语言），作为内核的沙箱化策略模块。
- AOK 宿主保持 Swift（Containerization framework 是 Swift API）；引擎任意语言。
- ABI 全程语言中立（fd/JSON-RPC + Zircon 式对象语义），P6 若需 Rust 内核子系统（形式化验证诉求）不改变 wire 语义。

## 10. 决议 D10：Application 为 Agent 生存而存在

AOK 的 Application 不是面向人的软件包或 Linux 进程包装，而是 Agent 为完成工作而创建、
组合、改变和退役的持久能力单元。系统的默认目标是维持 Agent 的连续性和可恢复性；用户
入口只能观察、批准和调试，不能成为 Application 继续工作的前提。

因此必须分开三层身份：

```text
agent_uuid       跨 boot 的 Agent 身份
application_id   Agent 所需的持久能力/状态单元
aproc AID        当前 boot 的可替换执行化身
```

Application 的 contract、tool binding、memory schema、wake policy、durable mailbox 和
checkpoint 进入 LSFS。worker crash、freeze、reap、engine 替换和 VM reboot 只重建新的
aproc/generation，不自动终止 Application。只有显式 retire 或不可恢复的策略终止才写入
tombstone，随后才允许可达性 GC。

Application 的三种变化必须具备版本和审计记录：

- **生长**：Agent 发现能力缺口，增加工具、记忆索引、worker 或资源预算；新增 capability
  必须经过父 capability 收窄和资源域检查。
- **迭代**：发布新的 contract version，并以 migration record 迁移未确认消息和可迁移状态；
  不兼容版本可以并存到旧 turn 完成。
- **消失**：停止新请求，完成 checkpoint，撤销 capability，写 tombstone，再回收不可达对象。

所有外部副作用使用 `turn_id` + `request_id` 幂等键，replay 不得重复已确认副作用。P1
验收必须包含 worker 崩溃恢复、VM 重启后 `application_id` 不变、旧 AID 不复用、无 client
连接时按 `wake_policy` 继续工作或休眠，以及 mailbox replay 不重复副作用。

## 11. 决议 D11：事件源与主动唤醒是一等 ABI

Proactive 是 AOK 区别于普通 Agent 编排框架的核心能力。事件源不是 P4 的外围触发器，
而是 P0 必须冻结的对象和调用语义：`timer` 负责时间，`port` 聚合 channel/eventpair/
pressure/supervisor 事件，`lsfs` 负责 commit 和 mailbox 变更。

事件投递目标是持久 `application_id`，不绑定某个临时 AID。runtime 先把事件写入 durable
mailbox，再按 `wake_policy` 处理：frozen 时恢复当前 aproc，dormant 时创建新的
aproc/generation，worker failed 时重建 worker 并 replay pending 事件。资源不足只能保留
pending，不能绕过资源域强行唤醒。

P0 固化 `event_source`/`port` 对象、`aok_event_source_create`、`aok_event_bind`、
`aok_event_ack` 和 `aok_event_read`；事件采用 level-triggered、单调序号、幂等
`event_id`、overflow 和 replay 语义。LSFS 持久化 binding，事件与 Application registry
在同一事务中启用或撤销。

## 12. 决议 D12：Agent-native ABI 与上下文效率

AOK 的默认用户不是人类桌面或 POSIX 程序，而是 Agent runtime。GUI、CLI、ACP 和 Linux/POSIX
兼容都位于 AOK 之外，作为可选观察、控制或迁移 adapter；它们断开时不能改变 Application 的
生命周期、事件投递或恢复语义。ComputerUse 只用于没有结构化工具接口的外部软件，不是原生
工具调用路径。

上下文是可寻址的工作集。AOK v1 固化 `ctx_as`、稳定前缀/可变尾部、hot/warm/cold 分层、
KV frame affinity、COW fork、`ctx.pagein`/`ctx.evict`、artifact handle 和带 hash/provenance
的 compaction manifest。大工具结果不得反复复制进 prompt；模型只接收摘要、schema、hash 和
受 capability 约束的 handle。恢复必须报告 `kv_exact`、`prefix_replay` 或 `text_replay`，
不能把部分恢复伪装为无损。

效率契约由 kernel/runtime 共同记录 `prompt_tokens`、`cached_tokens`、`pagein_bytes`、
`pagein_latency_us`、`tool_result_reuse_count`、压缩成本和 replay 成本。P1 验证 COW 与
artifact 语义，P2 按后端报告可复算的前缀命中率和 token 成本；后端私有日志不构成验收证据。

## 13. 决议 D13：动态模型路由

模型路由属于 L1 的 router system agent，kernel 不承担 provider 选择。router 根据任务
类型、上下文规模、隐私 capability、延迟目标、token/费用预算、backend health、工具需求
和 KV 亲和生成不可变 `route_record`；`ainf` 负责 capability 检查、资源预留、cache
compatibility、usage 对账和显式 fallback。Agent 的 hint 只能收窄候选或表达偏好。

KV 兼容键为 `model_id`、`tokenizer`、`chat_template`、`quantization`、`backend_version`
和 `device/backend`，不一致时必须报告 `prefix_replay` 或 `text_replay`。每个 turn 都要
记录 prompt/cached/completion tokens、pagein、延迟、fallback 和命中类型，保证路由成本
与缓存收益可复算。详细 ABI 见 [route-model.md](abi/route-model.md)。

## 14. 决议 D14：HostFS 与 Artifact Exchange

宿主与 Agent 通过 hostfs bridge 交换文件。virtiofs 提供 capability 授权的 live mount，
artifact import/export 提供按 hash 的不可变传输；两者都不把路径字符串当权限证明。默认
使用 `ro`、`dropbox-in` 或 `dropbox-out`，受限双向 `rw` 需要 single-writer 或显式版本
冲突语义。文件事件进入 event source，外部文件带 taint，导出经过 capability、大小/类型、
hash、幂等和审计检查。详细 ABI 见 [hostfs-model.md](abi/hostfs-model.md)。

## 15. 决议 D15：Human Message Gateway

消息网关是被 supervisor 管理的持久 Application。Telegram、Slack、邮件、Webhook、GUI
和 ACP 仅作为 adapter，把消息规范化、映射 external principal、转换附件为 artifact 后
写入 durable inbox/mailbox；Agent 自己决定唤醒、创建 turn、转交、记忆或请求确认。出站
使用有范围的 channel capability、幂等键、重试/backoff、dead-letter 和 delivery receipt，
断线或重启后按游标 replay 且不重复副作用。详细 ABI 见 [message-gateway.md](abi/message-gateway.md)。

## 16. 更新后的阶段计划

**P0 — ABI 冻结**（产出 `docs/abi/`，1 迭代）
- 对象模型规范：对象类型/rights 位集/channel/job 语义（D2）
- syscall 表 v1：旧 `spawn/wait/signal/ps` 只作为兼容映射；新增 job/aproc、open/close/duplicate/transfer、sched.hint、ctx.pagein、ai.*、ipc.*
- 引擎协议骨架（D7）、acap 令牌/策略 schema（D8）、LSFS schema（D6）、manifest→CapabilitySet 编译格式
- ACP v1 initialization、session/prompt/update/cancel/permission 的 adapter profile，明确 `sessionId`/AID/turn 的映射；采用固定 schema commit，不跟随 upstream `main`
- Application ABI：稳定 `application_id`、版本化 contract、wake/checkpoint/retention policy、durable mailbox、幂等副作用和 retire/tombstone 语义（见 [application-model.md](abi/application-model.md)）
- Event source ABI：timer/port/LSFS 事件、Application 绑定、wake policy、ack、去重、overflow 和 replay
- Context ABI：stable prefix/mutable tail、hot/warm/cold、KV affinity、COW、pagein/evict、artifact handle、compaction manifest、provenance、recovery level 和效率指标（见 [context-model.md](abi/context-model.md)）
- Dynamic Route ABI：route policy/record、资源预留、KV compatibility、fallback 和可复算 usage（见 [route-model.md](abi/route-model.md)）
- HostFS/Artifact ABI：virtiofs capability mount、双向交换、冲突、文件事件和 taint（见 [hostfs-model.md](abi/hostfs-model.md)）
- Message Gateway ABI：channel、identity、conversation、inbox/outbox、delivery、重试和 replay（见 [message-gateway.md](abi/message-gateway.md)）
- 两个 spike（写 spec 时验证）：biscuit-go v2 完备度（不达标则确认 acapd Rust 路线细节）；vsock dial/listen 跑通心跳

**P1 — 内核骨架**（引导路线 (a)）
Zircon 对象模型 + aproc 状态机 + VTC/MLFQ 调度 + freeze/恢复 + amem v1（消息页 + LSFS v0 SQLite）+ channel IPC + /proc 命名空间 + OTP 监督。echo 引擎迁到 D7 协议。
验收：COW fan-out 3 agent 且稳定前缀不复制；工具大结果只传 artifact handle；`/proc` 可 `cat` VTC 和 context 统计；freeze→resume 上下文无损；supervisor 重启崩溃 agent 只重建消息页。

Application 相关验收：worker 退出或 VM 重启后，Application identity、checkpoint 和未确认消息仍可恢复；
没有 CLI/GUI/ACP client 时，wake policy 仍按事件继续或休眠。

Proactive 相关验收：timer 唤醒 dormant Application；channel/port 事件恢复 frozen Application；
LSFS commit 事件进入 durable mailbox；重复投递只产生一次副作用；事件 overflow 后可从游标 replay。

**P2 — ainf 设备与真推理**（迁引导路线 (c)）
vsock 控制面 + 心跳；ainf 设备 + backend-metal（llama.cpp slots save/restore）+ backend-anthropic（断点/TTL/预热）；budget 记账（usage 字段→VTC）；cache 亲和 spawn。
router system agent 接入 route policy，验证显式 fallback、KV compatibility 和保守计费。
验收：同一 fd ABI 切换双后端；命中率可测；budget 超限触发节流→freeze；AOK 以 PID 1 起于自制 initfs。

**P3 — acap 强制**
acapd（Rust）+ Biscuit/Cedar/污点层；manifest→CapabilitySet→Landlock+seccomp 编译器；unotify 人工确认慢路径；Sigsum 审计链 + witness 联签。
验收：无 cap 外发被拒且审计可验；污点注入被拦或降级为确认；撤销两级生效。

**P4 — 系统 Agent 与协议驱动**
supervisor/fsd/ash（shell 即 agent，文件操作即 syscall）；MCP/A2A 驱动（每 MCP server 独立能力域）；host aok CLI 升级（exec → handle 语义）。
ACP adapter 与 engine protocol 保持独立：ACP 面向外部 client，engine protocol 只面向 runtime/backend。验收：ash 内完成 spawn/inspect/pipe；外部 MCP server 挂载与撤销闭环；真实 ACP client 完成多轮 prompt、流式 update、cancel 和 permission deny。
补齐 web.fetch/document/session/browser 分层、hostfs bridge、artifact transfer 和
message gateway。验收：消息进入 mailbox 后按 wake policy 唤醒 Agent，Agent 调用 Web 与
文件能力并将带 receipt 的回复送回原 conversation；断线重连不重复投递。

**P5 — MVP：多开并行调研（端到端）**
ash 一条命令：planner → COW fork N researchers（三后端混合推理）→ port 回传 → aggregator 写 LSFS → 全程 /proc 观测 + 审计链完整。
验收：报告落盘、VTC 公平性数据、cache 命中率报告、零越权事件。

**P6+ — 远期**：agent 级 checkpoint 与 VM 迁移（freeze→vsock 同步→恢复）；跨 VM A2A 网格；eBPF 观测桥；Rust 核心评估。

## 17. 风险登记表（新增/更新）

| 风险 | 等级 | 对冲 |
|---|---|---|
| VZ 无 snapshot API | 高（影响迁移/checkpoint） | agent 级状态同步方案（P6）；LSFS commit 天然是 checkpoint |
| vsock relay 并发 bug 史（#911/#2247） | 中 | 应用层心跳/重连；P2 迁离 vminitd；控制面消息幂等 |
| macOS 27 TCC 本地网络策略 | 中 | vsock 主控制面绕开；TCP 数据面仅可选 |
| biscuit-go 完备度未知 | 中 | P0 spike 定夺；备选 macaroon-bakery（Go 成熟）或 acapd Rust 已定 |
| 云后端 KV 不可控 | 中（影响 amem 页帧层） | 分级降级设计（本地强保留/云尽力而为已写入 D5） |
| modernc sqlite vec 性能（暴力 KNN） | 低 | 单 agent 记忆 <10 万条目内够用；量级到了外挂 Qdrant |
| engine 协议 v1 无标准背压 | 低 | token 流速率低；suspend/resume 显式原语 |
| virtiofs 双向写冲突 | 中 | 默认 single-writer/dropbox；版本冲突可观察，LWW 不作默认 |
| 宿主路径授权泄漏 | 高 | hostfs capability 绑定路径和 Application；bridge 二次校验；不可由路径提权 |
| artifact 重试造成重复副作用 | 中 | 内容 hash + owner/application 作用域 idempotency key + delivery record |
| gateway webhook 重放或身份伪造 | 高 | 签名校验、message id 去重、external principal 映射和审计 |
| channel rate limit / provider 失败 | 中 | adapter 限流、指数退避、dead-letter、outbox replay |
| 浏览器 session/cookie 泄漏 | 高 | cookie/profile 仅 handle；TTL、owner、taint 和 destination capability |
| 路由切换导致 KV 失效 | 中 | 完整 compatibility key；强制报告 prefix/text replay 与成本 |
