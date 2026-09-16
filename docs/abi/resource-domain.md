# AOK Resource Domain v1

资源域是 aproc 的可继承账户，负责 CPU、内存和 token 的可观测记账与超限处理。
本切片不选择模型后端，也不实现 sched_ext；它只提供内核可验证的预算闭环。

## Scope

账户层级为 `job -> aproc -> turn`。创建 aproc 时必须从父账户收窄上限；子对象不能增加
任一额度。额度为零表示该资源不受 AOK 预算限制，但内核安全上限仍然有效。

```c
struct aok_budget {
    __u64 size;
    __u64 cpu_usec_limit;
    __u64 memory_bytes_limit;
    __u64 token_reserve;
    __u64 token_hard_limit;
    __u32 flags;
    __u32 reserved;
};
```

## Accounting

CPU 使用量按 task 的用户态和内核态累计到 aproc；内存使用量读取其 task 集合对应的
memcg 当前值；token 只接受绑定到 aproc/session 的 usage channel 回报。每类计数器均为
单调递增，状态读取必须返回 limit、used 和当前 enforcement level。

token 采用 reservation 先行、usage 对账：reservation id 不可复用，usage report 带单调
`usage_seq` 与幂等 `usage_id`。缺少可信回报时按 reservation 上限计费，重复或乱序回报拒绝。

## Enforcement

每种资源独立判定，状态只能沿以下方向升级：

```text
within -> throttle -> freeze -> supervisor_event
```

恢复预算后可从 `throttle` 回到 `within`；`freeze` 只能由持有 SIGNAL capability 的调用者
显式 resume。OOM kill 默认关闭，只有对象策略明确允许时才可作为最终动作。

超限事件写入 aproc 有序事件环，包含资源类型、used、limit、level 和单调 sequence；事件
投递不改变 turn 或 checkpoint 语义。并发更新采用账户锁和饱和算术，不能因计数器溢出绕过限制。

## Planned ABI

`aok_budget_set(aproc_fd, budget)`、`aok_budget_get(aproc_fd, status)` 和
`aok_token_usage(aproc_fd, report)` 返回 fd ABI 的负 errno。正式 syscall 编号在 `0004`
补丁生成时分配；本文件先冻结结构和错误语义。
