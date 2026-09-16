# AOK P0 ABI Freeze Record

状态：`1.0-draft` 语义冻结。P1 对象切片已使用实验 syscall 编号和 UAPI 头文件；完整 ABI
的稳定编号仍待后续实现和评审。

这份记录把第一组内核实现不能自行解释的边界固定下来。实现可以增加能力探测字段，不能
改变这里的状态转移、记账规则或错误含义。

## 错误边界

AOK 调用优先返回标准 Linux `errno`，并使用扩展语义而不是新的数字空间。v1 的映射如下：

| 情况 | 返回 | 说明 |
|---|---|---|
| fd 类型或 rights 不匹配 | `-EBADF` / `-EPERM` | 不泄露对象是否存在 |
| capability 已撤销 | `-EKEYREVOKED` | 若目标架构没有该 errno，映射为 `-EPERM` 并在 event 中给出原因码 |
| 对象状态不允许操作 | `-EBUSY` | 例如运行中的 aproc 不能 reap |
| 请求尚未完成 | `-EAGAIN` | 非阻塞调用不改变对象状态 |
| 事件游标早于可重放范围 | `-EOVERFLOW` | 调用者必须先 inspect 快照 |
| 消息或结构超过上限 | `-EMSGSIZE` | 内核绝不截断 |
| 预算或配额不足 | `-EDQUOT` / `-ENOSPC` | 分别表示账户配额和对象/队列容量 |
| 请求被显式取消 | `-ECANCELED` | 只取消请求，不隐式销毁 aproc |
| 后端或可选扩展不存在 | `-ENOTSUP` | 不允许静默降级成另一种 wire 语义 |

错误返回不得包含 prompt、token、宿主路径或 capability 内容。细节通过带序号的审计/event
记录提供，并受 `INSPECT` right 约束。

## aproc 与 Linux task/pidfd

`aok_aproc_create()` 原子创建 aproc、内核生成的 AID 和首个 Linux task，并返回 aproc fd
与首个 task 的 pidfd。内核在 aproc 中保存 task 引用，而不是保存可复用的 PID 数字。

- task 的 `fork/clone` 子 task 默认继承 aproc；创建时不能通过普通 Linux syscall 清除归属。
- `aok_aproc_attach()` 必须同时提供目标 pidfd 和 `ATTACH` right。内核按 pidfd 解析活跃
  task，并检查 PID namespace、job policy 与 capability；PID 数字从不参与授权。
- v1 不提供无条件 detach。task 退出后仍由 aproc 记账，直到 supervisor 读取终态 event
  并执行 `aok_aproc_reap()`。
- task 退出、exec 或 pidfd 关闭都不销毁 aproc；aproc 的终态只由显式 abort、策略终止或
  supervisor reap 产生。
- AID/koid 在一次 boot 内单调分配且不复用。跨 boot 的 `application_id` 由 LSFS 保存，
  不能把 AID 当作持久主键。

## 事件队列、溢出与背压

所有 AOK event queue 是有界的、按 `event_seq` 单调排序并采用 level-triggered 语义。事件
包含 `event_seq`、`source_koid`、`application_id`、幂等 `event_id` 和类型化 payload。

当队列接近容量上限时，内核先产生一次 `AOK_EVENT_PRESSURE`，并停止继续扩大该队列。队列
满时：

1. `channel/port` 的生产者收到 `-EAGAIN`，不得丢弃或部分写入消息；
2. `timer` 只保留一个 pending 实例，并增加 coalesced 计数；
3. `lsfs/mailbox` 事件保留在 durable log，投递游标停留在未确认位置；
4. 对无法保留的瞬时事件，内核写入 `AOK_EVENT_OVERFLOW`，携带 `lost_from_seq` 和
   `replay_from_seq`，然后将对象标记为需要 inspect。

调用者从早于 `replay_from_seq` 的游标读取时得到 `-EOVERFLOW`。先读取 inspect 快照，再
从返回的 `replay_from_seq` 继续；重复 ack `(application_id, event_id)` 必须成功且不重复
产生副作用。事件源不能绕过冻结、节流、capability 或 supervisor policy，资源不足时只
保留 pending。

## CPU、内存和 token 压力

每个 `subos -> principal -> job -> aproc -> turn` 账户分别记账 CPU、内存和 token。默认
阈值为：

- 账户达到 80%：发送一次 advisory pressure event；
- 达到 100%：进入 `throttle`，后续工作只能消耗已批准的剩余额度；
- 超过硬上限并持续一个可配置 grace period（默认 1 秒）：`freeze` aproc 并发送
  `AOK_EVENT_BUDGET_FROZEN`；
- supervisor 收到冻结事件后决定恢复、追加配额或终止，并负责写审计记录。

token reservation 是硬上限：没有可验证 usage 回报时按 reservation 上限计费，不能等待
后端补报来解除冻结。CPU 和内存使用 cgroup/sched 统计；内存压力由 memcg pressure 事件
触发同一阶梯。OOM kill 默认关闭，只有 aproc/job 同时设置 `ALLOW_OOM_KILL` 且 capability
policy 允许时才可作为 supervisor 的最后动作。

阈值可以由父 job 收窄，不能由子 aproc 放宽。所有压力转移带 `usage_seq`，重复或乱序的
usage report 必须幂等处理或拒绝，不能回滚已经结算的账户。

## Token usage 对账

runtime 先调用 `aok_token_reserve()`，得到不可复用的 `reservation_id`。后端只能通过与
session/aproc handle 绑定的 usage channel 回报：

```text
usage_id, reservation_id, usage_seq,
input_tokens, output_tokens, cached_tokens,
timestamp_ns, flags
```

`usage_seq` 在 reservation 内严格递增；相同 `usage_id` 的重试必须返回同一结算结果。未知
reservation、跨 aproc 回报、回退序号和超过保守上限的 usage 都拒绝，并生成审计 event。
每个 turn 的 reservation 在 commit、abort 或 supervisor freeze 时封存，不能再次追加。

## P0 验收门槛

P1 patch 在进入 `kernel/patches/series` 前必须有独立 kselftest 覆盖：

1. aproc 创建、pidfd attach、fork 继承、task 退出后 reap，以及 AID 不复用；
2. queue 满、overflow、cursor replay、重复 ack 和三类 event source 的背压；
3. CPU/内存/token 三项从 advisory 到 throttle/freeze/event 的阶梯；
4. reservation usage 的乱序、重复、跨对象和缺失回报；
5. freezer 停止 task 后恢复，且没有 runtime checkpoint 时明确返回 `recovery=none`。
