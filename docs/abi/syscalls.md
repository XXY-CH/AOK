# AOK fd ABI and Resource Accounting

资源域的结构、记账和超限状态机见 [resource-domain.md](resource-domain.md)。

以下名称是语义名称。P1 对象切片的 470-472 仅为固定基线上的实验编号，其他调用的正式
编号待实现时分配。所有调用都返回负的
`-errno` 或 AOK 扩展错误；错误消息不能包含凭据或 token 内容。

## Object calls

| 调用 | 语义 | 最低 rights |
|---|---|---|
| `aok_job_create(parent_job_fd, attr)` | 创建子 job，继承并收窄父 policy/budget | `MANAGE_CHILD` on parent job |
| `aok_job_set_policy(job_fd, policy)` | 设置只能收窄的 job policy | `SET_POLICY` |
| `aok_aproc_create(parent_job_fd, attr)` | 创建 aproc、AID 和首个 task；返回 aproc fd 与首个 task pidfd | `MANAGE_CHILD` on parent job |
| `aok_aproc_spawn(aproc_fd, attr)` | 在已有 aproc 中创建额外 task，返回 pidfd | `MANAGE_CHILD` |
| `aok_aproc_attach(aproc_fd, pidfd)` | 把现有 task 加入 aproc | `ATTACH` |
| `aok_handle_duplicate(fd, rights)` | 单调收窄并复制 fd | `DUPLICATE` |
| `aok_handle_replace(fd, rights)` | 收窄并关闭旧 fd | `DUPLICATE` |
| `aok_object_inspect(fd, query)` | 读取 koid、类型、状态和统计 | `INSPECT` |
| `aok_object_close(fd)` | 关闭调用者引用；不等于销毁对象 | 无额外 rights |
| `aok_aproc_reap(aproc_fd)` | 回收 exited/failed aproc | `DESTROY` |

## Lifecycle and events

| 调用 | 语义 |
|---|---|
| `aok_aproc_freeze(aproc_fd, reason)` | kernel freezer 停止 task；幂等 |
| `aok_aproc_resume(aproc_fd)` | 恢复 task；没有 runtime 快照时返回恢复级别 `none` |
| `aok_aproc_abort(aproc_fd, turn_id)` | 取消当前 turn 和可取消工具；不销毁 aproc |
| `aok_ctx_pagein(aproc_fd, ref, priority)` | 将授权的 warm/cold segment 或 KV frame 引入 `ctx_as`，返回 pagein id |
| `aok_ctx_evict(aproc_fd, ref, mode)` | 释放 hot/warm 物理占用，保留可恢复引用和 hash |
| `aok_ctx_fork(aproc_fd, prefix_ref, attr)` | 以 COW 共享稳定前缀，创建新的 context root 和 aproc 关联 |
| `aok_ctx_commit(aproc_fd, turn_id, manifest)` | 原子提交 mutable tail、artifact manifest 和 checkpoint 引用 |
| `aok_ctx_compact(aproc_fd, manifest)` | 按 compaction manifest 压缩上下文并保留 hash/provenance 映射 |
| `read(aproc_fd)` | 读取完整有序 AOK event 记录；通过 `from_seq` 游标读取，不是普通字节流 |
| `poll(aproc_fd)` | 等待 event、peer close、overflow 或终态 |
| `aok_sched_hint(aproc_fd, hint)` | 提交 urgency、可延迟性和预期 turn 类别 |
| `aok_event_source_create(kind, attr)` | 创建 timer、port 或 LSFS event source，返回 fd | `MANAGE_CHILD` |
| `aok_event_bind(source_fd, application_ref, policy)` | 将事件源绑定到持久 `application_id` 和 wake policy | `WRITE` on source |
| `aok_event_ack(source_fd, event_id)` | 幂等确认事件；未确认事件可 replay | `WRITE` on source |
| `aok_event_read(source_fd, from_seq)` | 读取完整事件记录，支持 overflow 后重放 | `READ` |

`freeze`、`resume`、`abort` 与 ACP `session/cancel` 不同：ACP cancel 只请求结束当前
prompt turn；AOK freeze 停止所有绑定 task；abort 是内核对象上的取消操作。

### 已实现的内核事件切片

`0009-aok-event-wake.patch` 在原 timer fd 上增加 `poll` 和有界 port 通知源，
不新增 syscall 编号。此处是 arm64 原型 ABI，尚未连接 supervisor 的持久 registry。

| 操作 | 当前行为与权限 |
|---|---|
| `poll(source_fd)` | `READ`；当前 fd cursor 后有未确认事件时返回 `POLLIN`，无 `READ` 返回 `POLLERR`。读取推进 cursor，普通 `dup` 共享 cursor，AOK duplicate 创建新 cursor。 |
| `AOK_EVENT_SOURCE_PORT` (`kind=2`) | 创建时 flags、first_ns、interval_ns 必须为零；与 timer 一样最多保留 64 条未确认事件。 |
| `ioctl(source_fd, AOK_EVENT_POST, 0)` (`0xa021`) | `WRITE`；仅 port，入队一条无 payload 的通知。满队列返回 `EAGAIN`，不消耗 event ID/sequence，ack 后由发送方重试。 |
| `ioctl(source_fd, AOK_EVENT_SET_TARGET, &target)` (`0xa020`) | `WRITE` on source + `SIGNAL` on target aproc；原子绑定 identity、policy 与有引用保护的 aproc fd。 |

`struct aok_event_target` 固定为 16 bytes：`__u64 application_id`、`__s32 aproc_fd`、
`__u32 wake_policy`。`aproc_fd=-1` 配合 `MANUAL` 解除目标；关闭传入 fd 不解除已授权绑定。
原 `aok_event_bind` 只设置标签与 policy，并解除旧目标；数字 `application_id` 不授予唤醒权限。

有未确认或合并中的事件时，换 identity、目标或自动 policy 返回 `EBUSY`，避免旧事件被
重标或投给新目标；保留原 identity 并切到 `MANUAL`/解除目标仍允许。ack 的权限域仍是
source handle，而不是独立 Application registry。

timer/port 入队后通过 workqueue 恢复显式绑定的 `FROZEN` aproc；`ON_EVENT` 和
`ON_QUIESCENT` 在此切片均只恢复已有 frozen task。`MANUAL` 不恢复，资源超限造成的冻结
不自动恢复，failed/reaped 对象不会复活。显式授权 resume 保持原有语义。
该路径不创建 dormant Application、不恢复 checkpoint、不消费或 ack 事件，也不提供跨
进程/VM 的 durable replay。timer 满队列继续使用已有 coalesce 语义。

## Budget

资源账户层级为 `subos -> user principal -> job -> aproc -> turn`。v1 资源：

```c
struct aok_budget {
    __u64 cpu_usec_limit;
    __u64 memory_bytes_limit;
    __u64 token_reserve;
    __u64 token_hard_limit;
    __u32 flags;
    __u32 size;
};
```

token 记账分三步：

1. runtime/backend 用 `aok_token_reserve` 预留上限，返回不可复用的 `reservation_id`；
2. backend 通过只绑定到该 reservation/session/aproc 的 usage channel 回报 input/output/cache
   tokens；每条回报带单调 `usage_seq` 和不可复用的 `usage_id`；
3. kernel 以回报值对账，缺失或不可信时按 reservation 的保守上限扣费。

kernel 不解析模型私有 token 格式，也不接受未绑定 session/aproc 的 usage 回报。重复提交同一
`usage_id` 必须幂等，乱序或跨 reservation 的回报必须拒绝；对账差异进入审计事件。usage
channel 的认证由 kernel 绑定的 session handle 完成，不能只凭 JSON 中的 AID。

CPU、内存和 token 分别记账，但超限阶梯统一为：

```text
within budget -> throttle -> freeze -> supervisor event
```

默认不执行 OOM kill。只有 aproc/job 明确设置 `ALLOW_OOM_KILL` 且 capability policy
允许时，kernel 才能把 kill 作为最后动作。

## 外部迁移适配器（可选）

默认 AOK 镜像不依赖 `/run/aok/syscall.sock`。需要迁移现有 agentd 或 Linux 用户态工具时，
可以在用户态运行一个 JSON-RPC 适配器，将旧请求映射到 AOK 对象；该适配器不增加内核权限，
也不能把 POSIX 身份当作 Agent 身份：

- `spawn` 创建一个兼容 aproc 并返回旧 pid 映射；
- `wait` 读取终态 event；
- `signal=kill|term` 映射为 abort/终止；未知 signal 必须返回错误；
- `signal=suspend|resume` 在 freeze/resume ABI 上线前返回 `AOK_ENOTSUP`；
- `ps` 是 inspect 的快照，不授予 capability；
- 兼容面不能伪装成 ACP JSON-RPC，也不能传递 root capability。
