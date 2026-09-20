# 多 slot 联合测量验证

2026-09-20。P2 剩余项「多 slot 联合验证」与 P5「后端侧并行」的首个证据。
supervisor 侧 turn 并发（每应用单飞）由 Go 回归
`TestRunnerParallelTurnsFansOut`/`TestRunnerParallelSingleFlightPerApplication`
覆盖；本 smoke 补后端侧。

## 命令

`make runtime-mvp-multislot-smoke`（`runtime/scripts/mvp-multislot-smoke.py`）：

- llama-server 0.4.1：`-np 4 --ctx-size 512 -cb`（stories15M fixture，
  `--override-kv llama.context_length=int:2048`，每 slot 封顶 128 token）；
- supervisor：`engine: llama`、`-parallel-turns 4`、`-max-tokens 48`；
- 1 条 warmup turn 测单 slot 时延，随后 4 个应用并发 turn；
- 轮询 llama-server `/slots`（默认开启），统计同时 `is_processing` 的 slot 峰值。

## 结果（2026-09-20，本机）

```text
AOK_MVP_MULTISLOT=pass slots=4 parallel_turns=4 max_busy_slots=4
batch_ms=164 single_ms=79 rates=[0.93, 0.93, 0.0, 0.0] audit=20
```

- 验收断言：4 turn 全部 completed、无 deny、`max_busy_slots >= 2`
  （实测 4：后端真实重叠，非排队串行）；
- 批次 164ms 对单条 79ms（串行下界约 316ms），时间仅作参考，不作断言；
- 命中率混合（0.93/0.93/0/0）：多 slot 下 KV 复用是逐 slot 行为，
  本 smoke 不对 cache 作断言；单 slot 命中率证据见
  [RUNTIME-HITRATE-VALIDATION.md](RUNTIME-HITRATE-VALIDATION.md)。

## 边界

- stories15M 只证明联合执行与数据流，不证明报告质量；
- 未测 slot 数 > 并发 turn 数、slot 抢占与 `--cache-idle-slots` 行为；
- kernel-infer 路径单一 capability 串行（见
  [IMPLEMENTATION-STATUS.md](IMPLEMENTATION-STATUS.md)），多 slot 与 kernel ainf
  会话的联合（每并发 turn 独立 capability）属后续。
