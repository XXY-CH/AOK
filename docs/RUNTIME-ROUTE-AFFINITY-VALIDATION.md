# 路由记录与前缀亲和验证

2026-09-19，P2 切片：`PLAN-AOK-DEEP` 的"cache 亲和 spawn"与 router 显式
fallback / KV compatibility / 命中分类验证。

## 实现

- `RouterProvider` 按声明顺序 fallback，每个成功 turn 记录 `RouteInfo`
  （provider、`fallbacks` 计数、compatibility key）；全失败时记录为空。
- provider 声明部署级 `CompatKey`：llama/metal 按 endpoint（同一 server 即同一
  model/tokenizer/quant），anthropic 按 model+协议，echo 固定；未声明者退化为
  provider 名。
- `finishTurn` 把 route 写入 `TurnResult`（`provider`/`fallbacks`/
  `cache_hit_kind`），Application 持久化 `last_provider`/`last_compat`。
  `cache_hit_kind` 分类：后端回报 `cache_n>0` 为 `kv_exact`；同兼容键未命中为
  `prefix_replay`；兼容键变更为 `text_replay`（对应 route-model 的降级序列）。
- 调度亲和：`claimTurn` 在与最少计费候选的公平带（max(25%, 64 token)）内优先
  选择与上一完成 turn 共享 64 字符前缀的应用；带外不干预 token 公平。

## 证据

单测：`TestRouterRecordsRouteAndFallbackCount`（fallback 计数/空记录/compat 传递）、
`TestCacheHitKindClassification`（五类分类矩阵）、`TestFinishTurnRecordsRoute`
（结果与台账持久化）、`TestClaimTurnPrefixAffinity`（亲和生效 + 公平带约束）。

真实后端 smoke ×2（本机 llama.cpp 0.4.1 + stories15M-q4_0，单 slot，128-token
训练上下文——单个无关 turn 即可完全逐出 KV）：

```text
AOK_HITRATE_SMOKE=pass turns=2 hit_rates=0.00/0.99 cached_total=87
AOK_AFFINITY_SMOKE=pass shared_hit=87/88 kind=kv_exact other_kind=kv_exact
```

`make runtime-affinity-smoke` 归档输出到
`kernel/.build/smoke-logs/runtime-affinity.log`；脚本对同 tick 竞态共尝试三次（两次重试）。

亲和场景：应用 A 完成 shared 前缀 turn 预热；随后把无关前缀 turn（B）与 shared
前缀 turn（C）背靠背排队——C 后发。无亲和时 B 先执行并逐出缓存、C 必 miss；
实测 C 仍 87/88 命中且分类 `kv_exact`，证明调度把 C 拉到了 B 之前。

## 尚未完成

- `route_policy`/`route_handle` 对象模型、backend health 租约、跨 backend KV 迁移
  与 `ainf` 侧校验闭环；多 slot 命中率与 slot save/restore 的联合测量。
- vsock 控制面、Anthropic 真服务与 AFM 后端维持环境阻塞标注。
