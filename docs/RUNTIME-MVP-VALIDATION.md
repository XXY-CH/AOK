# MVP 多开并行调研验证

2026-09-20，P5 首证据：`PLAN-AOK-DEEP` P5 验收"一条命令多开并行调研"的
runtime 侧（echo 后端）。

## 流程

`make runtime-mvp-smoke`（`runtime/scripts/mvp-smoke.py`，输出归档
`kernel/.build/smoke-logs/runtime-mvp.log`）驱动**真实 supervisor + 控制面**：

```text
planner turn（共享 brief 前缀派生子任务）
→ N 个 researcher application 并行 fan-out（默认 3，AOK_MVP_RESEARCHERS 可调）
→ runner 按 VTC（tokens_used 升序）逐 turn 执行，前缀亲和调度生效
→ aggregator 收集结果、AGGREGATED 报告 turn 完成
→ finishTurn 把报告与 checkpoint 写入 context store（LSFS 侧落盘）
```

## 验收数据

```text
AOK_MVP_SMOKE=pass researchers=3 report_bytes=373 vtc_usage=[240, 240, 240]
hit_rates=[0.0, 0.0, 0.0] audit_records=20 denies=0
```

- **报告落盘**：aggregator 结果以 checkpoint 提交 context store（373 字节）。
- **VTC 公平性数据**：三个 researcher 的 token 台账逐 turn 可查（echo 下
  等量 240，真实后端下由 usage 回报拉开差异）。
- **命中率报告**：per-turn `cached_tokens/input_tokens` 从 `message.result`
  usage 直接可复算；echo 后端恒 0，真实 llama 下由
  `runtime-hitrate-smoke` 的同一机制给出非零值（0.00→0.99 已有独立证据）。
- **零越权事件**：整条链路的审计全为 allow（20 条记录），hash chain 由
  supervisor 每次 load 强制校验。

## 尚未完成

- 三后端**混合**推理（本证据为 echo；llama/Metal 已各自有独立证据，router
  混合编排属下一片）、/proc 观测（需 guest 内运行）、ash 一条命令入口、
  COW fork（amem 未实现，fan-out 当前为独立 application）。
