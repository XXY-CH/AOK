# Kernel P7 验证

`0007-aok-token-budget-enforcement.patch` 在 `0001`-`0006` 之后加入 token hard budget 的强制动作：超限报告先按保守上限记账，再产生 budget OVER 事件并冻结 aproc；冻结操作在释放 `aproc->lock` 后执行。

验证环境为 Linux 6.18.51、arm64 Debian builder 和 QEMU。启用配置结果：

```text
resource: 51/51
object:   44/44
task:     64/64
event:    37/37
core:     46/46
```

实验关闭配置下 resource disabled 测试为 `8/8`，所有 AOK syscall 返回 `ENOSYS`。资源测试覆盖 hard 超限后的保守账本值、FROZEN 状态事件、显式 resume、重复/乱序 usage、饱和计数和终态恢复。

patch SHA-256：`d0309640710cc3ab2e99e3f441e2a96438463644a559adc92c465a8599231192`。

边界：CPU 和 RSS 仍是观测值，没有内核 throttle；memory limit 不会触发 reclaim/freeze。`0007` 只闭合 token hard limit 的强制路径。
