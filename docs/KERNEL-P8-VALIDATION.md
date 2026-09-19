# Kernel P8 验证

`0008-aok-observed-resource-enforcement.patch` 在 `0007` 之后把公开 budget snapshot 中观察到的 CPU/RSS 超限转换为 fail-closed 冻结。RSS 计数同时按页大小转换为 `memory_bytes_used`，避免把内核页数误报成字节数。

QEMU arm64 验证结果：

```text
resource: 53/53
```

测试分别覆盖 CPU 和 RSS limit 为零时的观察超限、`OVER` 状态和冻结路径，并断言 RSS 观测值已从页数转换为字节；此前 token hard limit、预算记账、状态事件、显式恢复和终态恢复均保持通过。P8 依赖 `0007` 提供的 `aok_force_freeze`，不新增 syscall 或用户态 ABI。

边界：CPU/RSS 仍在 `aok_budget_get` 被调用时采样，尚不是每 tick 的调度器 throttle，也没有独立 memcg reclaim。实时 CPU 调度和内存压力强制仍是后续 P1 工作。


## 2026-09-19 追加：per-resource enforcement level（0011）

`0011-aok-resource-enforcement-levels.patch` 在 `aok_budget_status` 尾部保留字节内
加入 `cpu_level`/`memory_level`/`token_level`（WITHIN/THROTTLE/FREEZE；token 80%、
观测 CPU/RSS 90% 进入 THROTTLE，到限为 FREEZE），结构尺寸不变。resource kselftest
扩到 56 项：token 压力带报告 THROTTLE 且 aproc 保持 RUNNING、token 超限报告 FREEZE
并冻结、观测 RSS 压力带报告 THROTTLE。全套 enabled/disabled 矩阵、event probe 与
initfs boot 在修复 runner 的容器挂载陈旧缓存问题（同路径重写镜像后 QEMU 可能读到旧
页缓存，现一律从本地临时副本引导）后复验通过。runtime 侧 `claimTurn` 在同一压力带
执行准入节流（每应用每秒一个 turn），冻结仍为到达硬限的 fail-closed 行为。
