# Kernel P8 验证

`0008-aok-observed-resource-enforcement.patch` 在 `0007` 之后把公开 budget snapshot 中观察到的 CPU/RSS 超限转换为 fail-closed 冻结。RSS 计数同时按页大小转换为 `memory_bytes_used`，避免把内核页数误报成字节数。

QEMU arm64 验证结果：

```text
resource: 53/53
```

测试分别覆盖 CPU 和 RSS limit 为零时的观察超限、`OVER` 状态和冻结路径，并断言 RSS 观测值已从页数转换为字节；此前 token hard limit、预算记账、状态事件、显式恢复和终态恢复均保持通过。P8 依赖 `0007` 提供的 `aok_force_freeze`，不新增 syscall 或用户态 ABI。

边界：CPU/RSS 仍在 `aok_budget_get` 被调用时采样，尚不是每 tick 的调度器 throttle，也没有独立 memcg reclaim。实时 CPU 调度和内存压力强制仍是后续 P1 工作。
