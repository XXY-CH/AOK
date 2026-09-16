# AOK Context Model v1

## 目标

上下文是 Agent 的可寻址工作集，不是一次 prompt 的字符串。AOK 必须让 Agent 在不重复
发送大段历史的情况下恢复、分叉和继续工作。kernel 负责页、引用、预算和一致性；runtime
负责消息编排、摘要策略和模型特定的序列化。

## Context address space

每个 aproc 有一个 `ctx_as`。上下文由不可变 segment 组成，segment 以稳定 hash 寻址，
并通过 manifest 记录顺序、来源和生命周期：

| segment kind | 内容 | 默认可变性 |
|---|---|---|
| `system` | runtime/system contract | immutable |
| `identity` | Agent/Application 身份和策略摘要 | immutable per version |
| `tool_schema` | 当前 capability 可见的工具 schema | versioned |
| `task` | 当前任务和约束 | append-only |
| `memory` | 从 LSFS pagein 的记忆引用 | immutable |
| `tool_result` | 工具结果或 artifact 引用 | append-only |
| `summary` | 已压缩历史的摘要和 provenance | versioned |

上下文分为稳定前缀和可变尾部：

```text
stable_prefix = system + identity + tool_schema + pinned memory
mutable_tail  = task + recent messages + tool_result + summary
context_root  = hash(stable_prefix, mutable_tail manifest)
```

`stable_prefix_fingerprint` 必须随每次推理提交。只要前缀 fingerprint 未变，ainf 后端应
优先复用对应 KV frame；工具结果、时间戳和临时预算不能偷偷写入稳定前缀。

## Hot, warm and cold

- **hot**：当前 turn、最近可继续生成的 KV frame 和活跃尾部；保留在 ainf 后端可直接使用
  的 cache slot 中。
- **warm**：消息 segment、summary、近期 artifact manifest 和可快速恢复的 KV snapshot；
  由 amem 管理，可按 pressure 或 TTL pageout。
- **cold**：LSFS 中的 blob/tree/commit、向量/全文索引和完整 provenance；需要时通过
  `ctx.pagein` 恢复，不直接占用模型上下文窗口。

淘汰优先级由 Agent 工作流位置、下一次 wake 预期、KV TTL、引用计数和恢复成本共同决定；
LRU 只作为没有上下文信号时的兜底策略。

## Context operations

以下是语义调用名，实际通过 aproc fd 或 runtime 对应的 AOK syscall 暴露：

| 操作 | 语义 |
|---|---|
| `ctx.map(segment, position, flags)` | 将已授权 segment 映射到 `ctx_as` |
| `ctx.pagein(ref, priority)` | 从 warm/cold 引入 segment 或 KV frame，返回 pagein id |
| `ctx.evict(ref, mode)` | 释放 hot/warm 物理占用，保留可恢复引用和 hash |
| `ctx.fork(aproc_fd, prefix_ref)` | 以 COW 共享 stable prefix，生成新的 context root |
| `ctx.commit(turn_id)` | 原子提交 mutable tail、artifact manifest 和 checkpoint 引用 |
| `ctx.compact(manifest)` | 按 manifest 压缩 segment，保留 hash/provenance 映射 |
| `ctx.inspect()` | 返回层级、命中、pagein、evict 和恢复统计 |

`ctx.fork` 只复制 manifest 和引用计数；子 Agent 修改共享 segment 时才产生新 blob。fork
必须继承父 Agent 的 capability 上限和资源域上限，不能借 COW 获得新的权限。

## Tools and large results

工具调用返回小型结构化摘要和 `artifact_handle`，大结果写入受 capability 约束的 LSFS
object。后续 turn 只携带 handle、摘要、schema 和 hash；runtime 按需 pagein 原文。handle
必须绑定 `application_id`、`turn_id`、来源 taint、过期时间和读取 capability，路径字符串
不能替代 handle。

这样可以避免把网页、文件或命令输出反复复制进 prompt，同时保留可审计、可重放和可撤销的
读取语义。没有结构化工具接口的软件才通过外部 ComputerUse adapter 接入；ComputerUse
不是 AOK 的原生工具 ABI。

## Compaction and recovery

压缩提交必须产生 `context_compaction_manifest`，至少包含：

```json
{
  "before_root": "sha256:...",
  "after_root": "sha256:...",
  "replaced_segments": [{"hash": "sha256:...", "kind": "message"}],
  "summary_hash": "sha256:...",
  "provenance": [{"source_hash": "sha256:...", "range": [0, 42]}],
  "turn_id": "turn-7"
}
```

manifest 与 LSFS commit、durable mailbox 和外部副作用记录在同一恢复链中。恢复时 runtime
先验证 `after_root` 和 capability，再按缺失页执行 pagein；无法恢复完整 KV 时必须报告
`kv_exact`、`prefix_replay` 或 `text_replay` 级别，不能静默声称无损恢复。

## Efficiency contract

每个 turn 和每次恢复都记录以下指标，供 `/proc`、审计事件和 P1/P2 验收读取：

- `prompt_tokens`、`cached_tokens`、`output_tokens`
- `stable_prefix_fingerprint`、`kv_hit`、`kv_ttl_remaining`
- `pagein_bytes`、`evict_bytes`、`pagein_latency_us`
- `tool_result_reuse_count`、`artifact_bytes_read`
- `compaction_input_tokens`、`compaction_output_tokens`
- `recovery_replay_cost` 和 `recovery_level`

P1 至少证明 COW fork 不复制稳定前缀；P2 必须对本地和云后端分别报告前缀命中率、pagein
成本和 token 账单。缓存命中率不能只看后端私有日志，必须能通过 AOK 事件和 inspect 重新
计算。

