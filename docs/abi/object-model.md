# AOK Object Model

## 对象与句柄

AOK kernel object 由对象类型、koid、状态、引用计数和等待队列组成。用户态不直接携带
内核指针，只能通过 Linux fd 引用对象。fd 由 anon inode file 实现；关闭用户态最后一个
fd 不等于销毁对象，只有在 task、job、in-transit message 等内核引用也释放后对象才可进入
终态。在 channel 消息中的句柄属于 in-transit 引用。

v1 对象类型：

| 类型 | 作用 |
|---|---|
| `job` | aproc 的层级策略、配额、监督和子对象容器 |
| `aproc` | Agent 资源和生命周期对象，包含 AID、task 集合、budget、namespace 引用 |
| `channel` | 有序 datagram mailbox，支持句柄传递和 request/response |
| `port` | 多对象事件聚合和异步等待 |
| `eventpair` | 两端 peer-close 和状态通知 |
| `event_source` | 可绑定到 Application 的 timer、port 或 LSFS 变更事件 |
| `vmo` | 可共享的上下文、消息或后端数据块 |
| `session` | ainf 推理会话控制对象 |
| `tool` | 经 acap 授权的工具实例引用 |

`AID` 在一次 boot 内不可变、不可复用。跨重启持久身份由 LSFS 或宿主 principal 另行
保存，不能把 AID 当作永久数据库主键。LSFS 保存 AID 时必须使用固定 8 字节无符号表示，
不能依赖 SQLite `INTEGER` 的有符号解释。

Application 不是 v1 kernel object，而是由 runtime/libOS 和 LSFS 管理的 durable object。
`application_id` 是 LSFS 中稳定且不可复用的 UUID，用于跨 boot 标识 Application。它与当前
aproc 的 AID、Linux pid/pidfd、engine session id 和 ACP session id 分属不同命名空间。一个
Application 可以在不同时间拥有多个 aproc incarnation；aproc 或 task 退出不自动销毁
Application。Application 的 durable state、contract、mailbox 和 tombstone 由 LSFS 保存。

`event_source` 是主动唤醒的内核对象。它只描述事件产生和投递，不持有 Application 的永久
身份；绑定关系由 runtime/LSFS 保存。事件投递目标始终是 `application_id`，当前 aproc/AID
只是本次处理的执行化身。

## Rights

每个 fd 有独立 rights 位集。对象本身不因拥有某个 fd 而获得调用者权限；每个操作都必须
同时通过 fd rights、aproc capability table、job policy 和 Linux LSM 检查。

```
DUPLICATE  TRANSFER  WAIT  INSPECT
READ       WRITE     EXECUTE  DESTROY
ENUMERATE  SET_POLICY  ATTACH  SIGNAL
MANAGE_CHILD  MAP     RESIZE
```

`aok_handle_duplicate(fd, rights)` 和 `aok_handle_replace(fd, rights)` 只能删除 rights，
不能增加 rights。通过 channel 转移时可以删除 `DUPLICATE`，生成不可再委托的线性能力。
`TRANSFER` 缺失时禁止转移；`DESTROY` 缺失时禁止关闭以外的终止操作。

Linux `dup()` 保留原 fd rights；AOK 的显式 duplicate syscall 才能收窄 rights。`execve`
继承遵循 `FD_CLOEXEC`，AOK capability policy 可以强制所有 capability fd close-on-exec。

## Channel 语义

- endpoint 是线性能力：一个 endpoint 只有一个 owner fd，不提供隐式复制。
- 每条消息是 datagram；消息不能部分写入、不能重排。
- 句柄写入成功后由发送端消费；读端关闭时，在途消息和其中句柄一起销毁。
- peer 关闭在 `close()` 返回前对另一端可观察为 `AOK_EVENT_PEER_CLOSED`。
- v1 call 使用请求头中的 `txid` 匹配响应；通知没有响应。
- 大于实现上限的消息必须返回 `AOK_EMSGSIZE`，不能截断。

## Job 与 aproc

`job` 是策略和资源容器，`aproc` 是 Agent 级资源对象；二者不等同于 Linux process。
一个 aproc 可以管理多个 Linux task：

1. `aproc_create` 创建 aproc、AID 和首个 task，并返回 aproc fd 与该 task 的 pidfd；
2. 首个 task 获得的只是 aproc 自身的受限管理 capability，不是 kernel root capability；
3. 该 aproc 创建的子 task 默认继承 aproc；
4. attach 现有 task 必须同时持有目标 pidfd 和 `ATTACH` capability；
5. 跨 aproc attach 只能由 supervisor 或显式授权的 capability 完成；
6. task 退出不自动销毁 aproc，aproc 只有显式 close/reap 或策略终止才进入终态。

Job policy 和 quota 自上而下继承；异常事件沿 job 树向上发送。`BAD_HANDLE=KILL`、
禁止新建对象等策略是可选硬化策略，必须在能力探测中声明。

## Event source 与主动唤醒

事件源分为三类：

- `timer`：一次性或周期性 timer，触发时间由 kernel 单调时钟定义；
- `port`：channel、eventpair、资源压力和 supervisor 事件的聚合端口；
- `lsfs`：指定 namespace/ref 的 commit 变更或 mailbox 到达事件。

事件必须带 `event_seq`、`source_koid`、`application_id` 和幂等 `event_id`。投递采用
level-triggered 语义；未 ack 的事件保持 pending，可在 overflow 或重启后 replay。去重键
由 `(application_id, event_id)` 构成，重复 ack 必须幂等。

唤醒由 Application 的 `wake_policy` 决定：

```text
frozen aproc   -> resume existing incarnation
dormant app    -> create new aproc/generation
worker failed  -> restart worker, then replay pending events
budget denied  -> retain event in durable mailbox
```

事件源不能绕过资源域、capability 或 supervisor policy；资源不足时只记录 pending，不得
通过事件强行运行被节流或冻结的 Application。

## 生命周期

```text
created -> runnable -> running -> frozen -> runnable
                         |             |
                         +-> exited <--+
                         +-> failed
exited/failed -> reaped
```

`frozen` 只表示 kernel freezer 已停止 task。turn、KV 和 context 是否可恢复由 runtime
快照报告表示：`none`、`messages`、`messages+kv`。kernel 不承诺替 runtime 保存模型状态。

aproc 生命周期是执行化身生命周期，不是 Agent 或 Application 身份生命周期。Application
的生命周期见 [application-model.md](application-model.md)：worker crash、freeze、reap 和
VM reboot 都允许在新的 generation/AID 上恢复同一 `application_id`。

## 事件

每个 aproc 和 session 都有单调递增的 `event_seq`。事件包含 `aid`、可选 `turn_id`、类型、
时间戳和 payload。AOK anon inode 的 `read` 实现读取完整事件记录，不是普通字节流；调用者
通过 `from_seq` 指定游标，成功后返回实际消费到的最后一个序号。读端必须能通过 `poll/read`
消费；队列满时不得静默丢弃，必须发出 `AOK_EVENT_OVERFLOW`，返回最早可重放的序号，并将
对象置为需要重新 inspect 的状态。请求的游标早于该序号时返回 `AOK_EOVERFLOW`，调用者
必须先 inspect 再决定从快照继续，内核不伪造缺失事件。
