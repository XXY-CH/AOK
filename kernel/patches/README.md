# AOK Out-of-tree Patch Series

`series` 只列出已经存在、可以独立应用和验证的 patch。当前包含
`0001-aok-object-handle-core.patch` 与 `0002-aok-root-capability-bootstrap.patch`：QEMU arm64
上启用配置 43/43、禁用配置 4/4 测试通过，
并已从干净上游提交重建复验。证据见
[P1 验证记录](../../docs/P1-OBJECT-VALIDATION.md)。

补丁基线固定为 `f6388029ea9e2c9e807d73827658738ea131faee`（Linux 6.18.51）。
`kernel/linux` 保持上游 clean；构建脚本只在独立目录应用补丁。实验 ABI 的实际边界见
[p1-object-prototype.md](../../docs/abi/p1-object-prototype.md)。

P1 预定顺序：

1. `0001-aok-object-handle-core.patch`（已验证）：空 aproc/AID、anon-inode handle、rights 收窄和 inspect。
2. `0002-aok-root-capability-bootstrap.patch`（已验证）：PID1 一次性领取 root job capability，aproc 创建检查 `MANAGE_CHILD`。
3. `0003-aok-aproc-task-pidfd.patch`（已验证）：实现 task 创建/继承、pidfd attach、freeze、resume、abort 和 reap。
4. `0004-aok-resource-domain.patch`：CPU、memcg pressure、token reservation 记账和超限阶梯。
5. `0005-aok-event-source.patch`：timer、port、LSFS event、overflow、ack 和 replay。

每个 patch 必须同时更新 `kernel/kselftest/` 中对应测试，并记录通过测试时的 Linux
发行版、QEMU 版本和内核提交。未完成测试的 patch 不得提前写入 `series`。

在 Linux builder 上可用 `make aok-patch-check` 从固定 clean baseline 逐枚执行
`git apply --check` 和应用验证。脚本使用新的临时目录，不改动 `kernel/linux`。

GLM 返回的候选补丁先用 `make aok-candidate-check PATCH=kernel/patches/0003-...patch` 验收。
它会接在当前序列之后应用、拒绝越出 Linux 内核目录的路径、运行 `checkpatch`，但不会自动
加入 `series`。

资源域实现交接规格见 [GLM-HANDOFF-P3-RESOURCE-DOMAIN.md](../../docs/GLM-HANDOFF-P3-RESOURCE-DOMAIN.md)。
候选 `0004` 必须先通过 `make aok-candidate-check PATCH=kernel/patches/0004-aok-resource-domain.patch`，
再执行完整 series 构建和 QEMU 回归后才能加入本文件的 series。
