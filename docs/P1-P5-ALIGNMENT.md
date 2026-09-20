# P1-P5 与原始计划对照

2026-09-20，依据当前源码、回归测试及两份原始计划核对。
结论：**目标方向一致，P1-P5 均部分实现；现有切片通过不等于阶段完成。**
本次修复不修改原始验收标准，也不把用户态替代路径认定为目标内核机制。

原始依据：[PLAN-L0-AGENT-KERNEL.md](PLAN-L0-AGENT-KERNEL.md) 第 5 节、
[PLAN-AOK-DEEP.md](PLAN-AOK-DEEP.md) 第 16 节。当前证据索引见
[IMPLEMENTATION-STATUS.md](IMPLEMENTATION-STATUS.md)。

| 阶段 | 原始设想 | 当前实现 | 尚缺验收 |
| --- | --- | --- | --- |
| P1 | aproc、VTC/MLFQ、amem COW、IPC、/proc、监督恢复 | root/aproc/task/freezer/resource/event/registry 内核切片；PID1 引导；用户态 CAS/COW、mailbox/checkpoint 恢复；application.fork COW 扇出（页共享+污点继承，2026-09-20） | kernel amem/LSFS、sched_ext Agent 调度、内核语义 COW、/proc 统计、guest 内监督重启演练、独立 fsd |
| P2 | ainf 同一 fd ABI、真实后端、vsock、token budget、亲和放置 | llama/Metal/Anthropic provider、router、usage/cache/亲和；0006 ainf 与 KernelInferProvider probe 闭环；生产 engine 经 kernel ainf（`-kernel-infer`，guest 真内核 turn 验收 2026-09-20） | vsock 心跳、AFM、cache-affinity spawn、多 slot 联合验证、同 fd ABI 多后端切换验收 |
| P3 | acapd、Biscuit/Cedar、内核能力门、污点、确认、审计/witness | 用户态 capability、taint gate、确认、撤销、审计 hash chain 和本地 witness；内核推理 capability/revoke；manifest→Landlock+seccomp 编译器（sandbox.Compile，2026-09-20） | Rust acapd、Biscuit/Cedar、完整内核强制门、unotify、外部 witness 服务、guest 内端到端强制验收 |
| P4 | supervisor/fsd/ash、ACP/MCP/A2A、host CLI、web/hostfs/gateway | supervisor、web fetch/document、hostfs 授权/传输数据模型、gateway 持久队列与控制面；Swift TUI 经 aokctl 接控制面 | ash、独立 fsd、ACP/MCP/A2A、真实外部 transport、host exec → handle 语义迁移、web session/browser |
| P5 | ash 一条命令、COW researchers、三后端并行与工具、port 回传、LSFS、/proc 与审计 | shell 入口、COW researchers（context 页共享）、并行 turn 执行（每应用单飞）、持久报告；echo 与单 llama 数据流验证 | 三后端、工具调用、port 回传、完整 LSFS/fsd、ash、/proc 和公平性验收、多 slot 后端并行 |

P2 的要求有一项原始文档自身的演进：L0 计划要求 Metal/cloud/AFM 三后端，Deep
计划将 P2 门槛改为 Metal + Anthropic 双后端，但 **P5 仍明确要求三后端混合**。
因此双后端 provider 的存在既不能代替同 fd ABI 的集成验收，也不能替代 P5 三后端验收。

## 本次修复与目标的关系

- 网关重启按 channel + external conversation 精确恢复来源；Reply 检查应用、绑定、channel 与污点，撤销 channel 后拒绝领取 outbox。
- 撤销表满时拒绝新增并审计，不再删除仍有效的旧撤销；尚未实现过期回收。
- Router 遵循应用允许后端的顺序；WebExecute 在触网前验证应用，网络返回后复查。
- 内核事件按 boot 区分身份并在 ACK 前持久化，回滚重建可选 map 和来源索引；跨 VM 的保存仍由 supervisor 承担。
- P5 消费真实 planner 输出，由 aggregator provider 生成报告；默认保留结果和 state，支持复用 supervisor。
- 2026-09-20 第二轮：生产 supervisor `-kernel-infer` 把 engine turn 包入内核 ainf
  会话，guest 真内核引导验收（`make aok-initfs-turn-test`）；`application.fork`
  实现 COW 上下文扇出（跨 owner、页共享、污点继承、独立预算），mvp_flow 的
  researchers 经 fork 创建并以 `context.pages` 验证共享；runner 支持有界并行 turn
  （`-parallel-turns`，认领排序/VTC/节流/每应用单飞不变）；`sandbox.Compile`
  把 manifest 编译为 Landlock+seccomp launcher 计划并成为 `-engine-command`
  生产入口。

这些修复纠正已有实现的行为，并使它更接近原始设想；它们没有补齐上表中未实现的系统。
P5 详细验证与限制见 [RUNTIME-MVP-VALIDATION.md](RUNTIME-MVP-VALIDATION.md)。

## 本次验证

- `make runtime-test`：全量 Go race 测试与 vet 通过；Linux arm64 交叉编译通过。
- `make runtime-smoke runtime-core-smoke runtime-mvp-smoke`：引擎握手、crash replay、timer、LSFS 恢复及报告保留/重启读取通过；mvp smoke 以 `-parallel-turns 3` 运行，receipt 记录 `cow_shared_pages=[1, 1, 1]`。
- `make runtime-mvp-mixed-smoke`：真实 llama 单后端完整数据流通过，researchers 经 COW fork（`cow_shared_pages=[1, 1, 1]`），前缀命中率保持（0.95/0.95）；小模型 fixture 只验证接口与缓存，不证明研究报告质量或三后端混合。
- `make aok-event-probe-test aok-initfs-boot-test`：真实内核事件、supervisor 同 boot 重启、推理预算及 PID1 启动通过；启用 proc sysctl 后 appregistry 47、resource 56、core 46 项回归通过。
- `make aok-initfs-turn-test`（新增）：生产 supervisor 入口经内核 ainf 设备完成完整 turn，串口 `AOK_BOOT_TURN=pass provider=kernel-infer/echo input_tokens=19 checkpoint=<hash>`、`AOK_KERNEL_INFER=on`、READY 恰一次、串口干净。
- 并行语义回归：`TestRunnerParallelTurnsFansOut`（三应用 barrier 同时执行）与 `TestRunnerParallelSingleFlightPerApplication`（同应用最大并发 1）；COW 回归：`TestForkApplicationSharesContextPages`、`TestForkApplicationInheritsTaint`。
- 新旧 boot 编号碰撞、ACK 失败、恢复持久化失败与 producer 竞争由 fake bridge 回归覆盖；本次没有完成真实跨 VM 崩溃恢复联合验收。

旧版状态无 boot 身份；升级须有序关闭 supervisor 并完整重启 VM，不支持仅热重启
supervisor 的精确一次保证。详见 [升级约束](KERNEL-APP-REGISTRY-VALIDATION.md)。

## 后续完成判据

继续按原始计划逐项给出对应证据：P1 的 COW/调度/监督，P2 的设备与多后端，P3 的
内核强制边界，P4 的真实协议和系统 Agent，最后在 P5 同一流程中联合验收。
仅测试通过、token 台账相同、无 deny 或单后端缓存命中，都不能单独宣告整个阶段完成。
P0 文档仍为 `1.0-draft` 语义草案；稳定 syscall 编号和两个独立实现互操作也尚未验收。
