# P1 Object Prototype

本文记录 `0001`/`0002` 的历史边界，不代表当前完整 patch series。
最新实现与验证见 [IMPLEMENTATION-STATUS.md](../IMPLEMENTATION-STATUS.md)。

前两枚补丁是对象和 bootstrap 实验，不是 P0 产品 ABI 的完整实现。设计中的 job、
首个 task/pidfd、持久生命周期、事件和资源域仍按原决议实现；不能把此实验用于运行不可信
Agent。启用项为 `CONFIG_AOK_EXPERIMENTAL`，依赖 arm64 和 `EXPERT`，默认关闭。

## 当前接口

基线固定为 Linux 6.18.51，提交 `f6388029ea9e2c9e807d73827658738ea131faee`。
以下 470–473 是这个 fork 的实验编号，尚未分配为上游或稳定 ABI：

| 编号 | 原型调用 | 结果 |
|---|---|---|
| 470 | `aok_aproc_create(args, info)` | 分配空 aproc 对象，返回 CLOEXEC fd；不创建 task |
| 471 | `aok_object_inspect(fd, info)` | 返回 AID、类型、CREATED 状态和本句柄 rights |
| 472 | `aok_handle_duplicate(fd, rights)` | 返回独立、权限不增加的 CLOEXEC 句柄 |
| 473 | `aok_root_cap_claim()` | 初始 PID 1 一次性领取 root job capability |

`aok_root_cap_claim()` 只允许初始 PID namespace 中的 PID 1 成功一次；重复领取返回
`EALREADY`。创建时 `parent_job_fd` 必须是持有 `MANAGE_CHILD` 的 root/job capability，
否则返回 `EBADF`、`EINVAL` 或 `EACCES`。capability fd 按 Linux fd 规则 fork 继承、exec
关闭；内核不依据 UID 0、Linux `CAP_*` 或嵌套 namespace 的 PID 1 授权。该切片仍未创建
首个 task，随后 task/pidfd 切片再实现 P0 生命周期语义。

`args.size` 和 `info.size` 都由调用方预先填写。结构大小分别为 40 和 56 字节，最小大小
就是当前结构大小，最大为 guest `PAGE_SIZE`。新增输入尾部仅接受全零，否则 `E2BIG`；
输出尾部清零。未知 flags、非零输入 reserved、过短或过长大小返回 `EINVAL`，无效内存
返回 `EFAULT`。创建失败不发布 fd，但可能消耗 AID，因此 AID 允许跳号。

AID 从 1 单调分配到 `S64_MAX`，随后永久返回 `EOVERFLOW`，在一次 boot 内不回绕或复用。
跨 boot 身份仍使用 Application 的 durable ID。原型对象只被句柄持有，最后一个句柄
关闭后释放；未来 task/job/mailbox 的内核引用尚未加入，不能把这一行为当作 aproc reap。

## 权限与生命周期

原型实现 `INSPECT`、`DUPLICATE` 和 root capability 的 `MANAGE_CHILD`。权限属于 open file description，AOK duplicate
创建新的描述；Linux `dup/fork` 共享同一描述、保留同一份不可变授权。移除 `DUPLICATE`
只禁止 AOK 显式 duplicate，不能阻断 POSIX `dup`。禁止扩权以及缺少所需权限均返回
`EACCES`；错误 fd 返回 `EBADF`，非 AOK fd 返回 `EINVAL`。

POSIX fd 传递和继承目前仍按 Linux 规则运行，本补丁不宣称 `TRANSFER`、线性能力、
撤销、LSM/acap 强制或 job policy 已成立。AOK 产品 ABI 的能力传递约束需要后续内核
强制点。对象不会运行 task，不提供 freeze，也不宣称 Agent 已经持久存活。

## 验证合同

独立 kselftest 在 QEMU guest 的初始 PID 1 执行，调用实际新增 syscall。检查结构验证、
权限收窄、fd 耗尽和用户内存故障后的清理、并发句柄引用、fork/exec、嵌套 PID namespace
拒绝以及 AID 不复用。关闭配置后，同一测试程序的 `--expect-disabled` 模式要求四个
syscall 全部返回 `ENOSYS`。这仅完成第一枚补丁验收，不代替 P0 的完整 P1 验收列表。
