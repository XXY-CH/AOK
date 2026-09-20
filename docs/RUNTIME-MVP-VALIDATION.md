# MVP 用户态调研流程验证

2026-09-20。当前验证的是 P5 的用户态数据流切片，尚未达到原始计划的完整
“多开并行调研”验收。原始要求和差距见 [P1-P5-ALIGNMENT.md](P1-P5-ALIGNMENT.md)。

## 流程与持久结果

```text
planner 生成 N 个研究方向
→ 完整 plan 文本和分工编号传给经 application.fork 创建的 researcher applications
   （COW：子上下文引用 planner 封存 CAS 页，receipt 记录 cow_shared_pages）
→ 多条消息入队，runner 按 token 台账和前缀亲和认领，self-built supervisor 以
   `-parallel-turns 3`（AOK_MVP_PARALLEL 可调）并发执行 turn；每应用单飞
→ researcher 实际输出传给 aggregator，由 provider 生成报告
→ context checkpoint + report.txt + receipt.json
```

入口：`sh runtime/scripts/mvp-research.sh [-j N] [-o output-directory] "question"`。
未指定 socket 时自动启动 echo supervisor；指定 `AOK_CTL_SOCKET` 时复用其 provider。
默认结果保留在 `runtime/.build/research/run.XXXXXX/`，也可用 `-o` 指定目录。
自建 supervisor 的 state、manifest 和日志一并保留；脚本结束只清理临时二进制和
自身启动的进程。复用外部 supervisor 时不会终止它。

`receipt.json` 包含 planner、researcher、aggregator 的完整结果、checkpoint、usage、
命中率和本次应用的审计计数。报告先 fsync 再读回核对，不再依赖 `AGGREGATED:` 前缀。

## 回归证据

`make runtime-mvp-smoke` 调用真实 shell 入口，检查：

- planner 输出与分工进入 researcher，researcher 输出进入 aggregator；mock 回归使用任意报告文本。
- 脚本退出后报告与 receipt 仍在，重启保留的 supervisor state 后，`message.result` 与报告结果一致。
- 同一 supervisor socket 可再次完成流程，复用实例继续存活。
- 非法 researcher 数在创建应用前拒绝。

Echo 测试中三个 researcher 的 token 台账为 `[584, 584, 584]`，命中率为 0，
本次应用审计记录 20 条且无 deny。这是数据流和持久性的证据，不是报告质量、
调度公平性或完整安全边界的证明。

`make runtime-mvp-mixed-smoke` 保留历史 target 名，实际是**单 llama.cpp 后端**测试。
本轮结果：

```text
AOK_MVP_MIXED=pass scope=single_backend full_flow=pass backend=llama.cpp rates=[0.0, 0.95, 0.95] nonzero=2/3 audit=12
```

先验证三个共享前缀 turn 的缓存命中，再复用同一真实 supervisor 跑完整 shell 流程。
后者三个 researcher 的 token 台账为 `[100, 100, 100]`、命中率为 `[0.16, 0.62, 0.62]`，
报告 provider 为 `llama.cpp`，文件内容与 receipt 一致。默认 stories15M fixture 的训练
上下文只有 128 token，本测试用 `--override-kv llama.context_length=int:2048` 扩展测试
上下文以容纳完整输入；它不提供调研质量证据。用户提供的模型不修改 metadata。
无模型或 server 时 target 明确输出 skip，不能计为真实模型通过。

## 未满足的原始要求

尚无 ash 入口、三后端同时混合、工具调用、port 回传、
完整 kernel amem/LSFS/fsd 和 `/proc` 调度统计。COW 已在用户态 context 层实现
（提示词组合仍是文本重放，内核 amem 语义 COW 未实现）；并行是 supervisor 侧
turn 并发（每应用单飞），多 slot 后端侧并行未验证。当前 checkpoint 是用户态 CAS/SQLite，
结果通过 control UDS 的 `message.result` 轮询收集。token 台账尚不足以证明 VTC 公平性；
happy path 没有 deny 也不能替代主动越权测试和内核强制边界验收。
