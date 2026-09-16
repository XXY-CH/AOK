# AOK Dynamic Route ABI v1

状态：P0 设计初稿。路由是 L1 的 system Application；`ainf` 只执行内核可验证的会话、能力、资源和缓存约束。

## 目标

一次 turn 的模型选择必须可解释、可复算并可恢复。Agent 可以提供 hint，但不能把任意
provider、模型或宿主凭据当成自己的权限。最终选择由 router system agent 根据 policy
生成，并由 `ainf` 校验后返回 route handle。

## 路由输入

```json
{
  "task_class":"research",
  "context_tokens":42000,
  "required_tools":["web.fetch","lsfs.write"],
  "privacy":"confidential",
  "latency_target_ms":8000,
  "token_budget":{"input":50000,"output":8000},
  "hint":{"quality":"high","backend":"local-metal"}
}
```

Router 必须同时评估 `model_id`、tokenizer、chat template、quantization、backend version、
device/backend、上下文大小、隐私 capability、资源域余额、价格、延迟和 backend health。
`hint` 只能收窄候选集或表达偏好，不能提升 capability 或预算。

## 对象与生命周期

- `route_policy`：Application 版本的一部分，声明允许的 backend、模型、地域、费用上限、
  隐私级别和 fallback 顺序。
- `route_record`：每个 turn 的不可变决策记录，含输入摘要 hash、候选集、最终 backend、
  model、policy 版本、理由码、时间和资源预留。
- `route_handle`：绑定一个 `ainf Session` 的短期 capability handle；关闭 turn 或撤销
  capability 后失效。
- `backend_health`：由 backend supervisor 发布的租约状态，不得被 Agent 伪造。

路由顺序为 `admit -> reserve -> select -> open session -> usage reconcile`。预留失败时
不得偷偷换用未授权 backend；只能按已声明 fallback 重新准入或返回 `AOK_EQUOTA`。

## 缓存兼容与降级

KV 只有在以下兼容键全部一致时才可 `kv_exact` 复用：

```text
model_id | tokenizer | chat_template | quantization | backend_version | device/backend
```

否则恢复级别必须按 `kv_exact -> prefix_replay -> text_replay` 降级，并在 route record 和
context manifest 中记录原因。跨 backend 路由不传递原始 KV；只传递稳定前缀、artifact handle
和可验证的 replay manifest。

## 计费与指标

kernel 先按 token 配额预留；turn 完成后使用 backend usage 回报对账，无法回报时按声明的
保守上限计费。每个 route record 至少记录 `prompt_tokens`、`cached_tokens`、
`completion_tokens`、`pagein_bytes`、`latency_ms`、`fallback_count` 和 `cache_hit_kind`。

路由切换、cache miss、预算拒绝和 backend 不健康都必须产生日志事件。backend 私有日志
不能替代 kernel 可读取的对账记录。
