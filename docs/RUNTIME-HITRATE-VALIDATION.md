# 前缀命中率验证

2026-09-19，P2 切片：`PLAN-AOK-DEEP` 验收项"命中率可测"——命中率必须按后端
usage 报告可复算，后端私有日志不构成证据。

## 实现

- llama provider 已把 `timings.cache_n` 解析为 `Usage.CachedTokens`，并强制
  一致性（`cache_n <= tokens_evaluated`、`prompt_n == evaluated - cache_n`），
  缺少 cache 计账的后端直接报错而非估算。
- `finishTurn` 把每 turn 的 `CachedTokens` 累计进 Application 的
  `tokens_cached` 台账；per-turn 结果（`message.result`）与
  `application.inspect` 都携带原始 usage 字段，命中率
  `cached/input` 可在任意时间点复算，无需后端日志。
- 内核 `aok_token_usage` 的 `cached_tokens` 字段与 `aok_budget_status` 的
  `tokens_cached` 早已定义；本切片补齐的是 supervisor 侧的持久累计与暴露。

## 证据

`make runtime-hitrate-smoke`（`runtime/scripts/hitrate-smoke.py`）：本机
llama.cpp 0.4.1 + 仓库 stories15M-q4_0 模型（CPU，单 slot），真实 supervisor +
control API 驱动两个共享 ~88-token 前缀的 turn：

```text
AOK_HITRATE_SMOKE=pass turns=2 hit_rates=0.00/0.99 cached_total=87
```

`make runtime-hitrate-smoke` 会把输出归档到
`kernel/.build/smoke-logs/runtime-hitrate.log`。

第二个 turn 复用 87/88 个前缀 token（`timings.cache_n` 报告），台账累计 87。
llama-server 或模型缺失时脚本显式输出 `AOK_HITRATE_SMOKE=skip`。注意：该模型
训练上下文为 128 token，服务端会硬性封顶 slot 上下文，脚本的前缀长度据此选择。
单测 `TestFinishTurnAccountsCachedTokens` 覆盖台账与 per-turn 结果的传播。

## 尚未完成

- cache 亲和 spawn（同前缀 fan-out 的放置决策）与 router 的 KV compatibility
  键验证闭环尚未实现；多 slot 命中率与 slot save/restore 的联合测量待
  cache 亲和切片。
