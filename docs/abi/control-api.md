# AOK Control API

## 用户入口

`aok` CLI 是第一入口；外部 GUI 使用同一个 control API 和 event stream。两者都不直接
调用 Linux syscall，也不持有 kernel root capability。

用户身份由 host 登录身份映射为 AOK user principal。PID1 supervisor 持有 kernel root
capability，按 principal、manifest 和 policy 委派受限 capability。

## Control methods

control API 可使用本地 UDS 或宿主与 VM 之间的 vsock，方法语义与内核 fd ABI 分离：

`session.create`、`session.prompt`、`session.cancel`、`aproc.inspect`、`aproc.freeze`、
`aproc.resume`、`aproc.reap`、`budget.get`、`events.subscribe`、`permission.list`、
`permission.approve`、`permission.deny`，以及 `application.inspect`、`application.freeze`、
`application.resume`、`application.update`、`application.retire`、`event_source.create`、
`event_source.bind`、`event_source.list`、`event_source.disable`、`route.inspect`、
`route.policy`、`hostfs.mount`、`hostfs.unmount`、`artifact.import`、`artifact.export`、
`message.channel.create`、`message.subscribe`、`message.send` 和
`message.delivery.inspect`。

`application.export`（污点外发门控：拒/`confirm` 同步放行/`escalate` 进入慢路径/
`request_id` 凭已批准确认单次放行）、`confirmation.list`/`confirmation.settle`
（人工确认队列）也已实现。目标规格中的 `route.policy`/`route.inspect` 已以 `application.set_route_policy`（设置版本化 backend fallback 策略）与 `route.list`（倒序返回路由记录）实现。

当前本地控制实现还提供只读的 `application.list`、`mailbox.list` 和 `event_source.list`，供客户端展示。
`event_source.list` 默认仍返回 timer 数组；显式传入 `source_kind: "lsfs"` 才返回 LSFS binding 数组。

Application control 方法操作持久 `application_id`，再由 supervisor 选择或创建当前 aproc
incarnation。`aproc.*` 方法只操作当前 boot 的执行化身；两者不能互相替代。

`event_source.*` 方法创建或管理 timer、port 和 LSFS 变更源，并把它们绑定到持久
`application_id` 的 `wake_policy`。控制面断开不会删除绑定或清空 pending 事件；撤销绑定
必须经过 supervisor capability，并写入审计事件。

每个请求绑定 principal 和审计 correlation id。control API 只能使用 supervisor 委派的
管理 capability；越权返回 deny，不返回底层 root capability 的细节。

### 当前用户态 LSFS wake v0

以下是本地 Go supervisor 已实现的控制子集。socket 的 0600 权限及 peer identity 限定
同一宿主用户的管理访问；这还不是上述完整 capability 委派或多租户控制面。

| 方法 | 参数与行为 |
| --- | --- |
| `event_source.create` | `application_id`、`source_kind: "lsfs"`、`binding_id`；`source_ref` 默认取目标 Application context，必须属于相同 owner；`after_cursor` 默认 0，只扫描大于该 cursor 的记录。重复 binding ID 返回错误。 |
| `event_source.list` | `application_id`、`source_kind: "lsfs"`；返回按 `binding_id` 排序的 `binding_id/application_id/source_kind/source_ref/cursor/enabled` 快照。 |
| `event_source.disable` | `application_id`、`source_kind: "lsfs"`、`binding_id`；保留 cursor 和已入 mailbox 的事件，停止后续扫描。 |
| `event_source.bind` | 同上；重新启用已有 binding，从保存的 cursor 补扫，不能变更 source 或重置 cursor。 |
| `artifact.import` | `application_id`、`idempotency_key`、JSON `payload`；写入目标 Application context，返回内容 hash `handle`。同 key 同字节重试不产生第二条 commit。 |

`source_kind` 缺省或为 `timer` 时，create/list 保持现有 timer 契约；其他 kind 返回错误。
timer 的 bind/disable 与 port 源尚未实现。LSFS 每个 Application 最多 64 个 binding，
每轮每个 binding 扫描最多 64 条 commit，按 cursor 顺序推进。

v0 只投递 `artifact` commit；append/checkpoint 等记录只推进 cursor，避免 runner 的
写回再次唤醒自己。事件包含 `source_kind`、`binding_id`、`commit` 元数据和供 provider
读取的 `text` 通知，不会把 artifact 内容自动拼入 prompt。内部 mailbox 幂等键为
`lsfs:<binding_id>:<cursor>`；timer 使用 `timer:<timer_id>:<due>`。
`message.send` 禁用 `lsfs:`、`timer:` 和 `kernel:` 前缀。内核 registry 接线后
（PID1 认领 root 并经 `AOK_ROOT_FD` 交给 supervisor），artifact commit 先投递到
application 的内核 durable 队列（携带 cursor），drain 阶段以
`kernel:<application_id>:<event_id>` 幂等键进入 mailbox；满载保留 cursor 的语义不变。

mailbox 与 cursor 在同一 supervisor state 写入中提交；context DB 的 commit 先持久化，
扫描或进程失败后可以重读。artifact 导入后的审计写入是另一笔事务：若它失败，artifact
可能已经持久化，调用方应使用相同 key/payload 重试。manual/frozen 状态保留 pending，
不会自动解除冻结；retire 原子停用 binding 并将未确认 mailbox 标为 `expired`。
现有 mailbox/commit 还没有 retention 或磁盘配额。

### Mailbox 容量与源端重试

`pending` 和 `claimed` 合计受两级限制：每个 Application 256 条、2 MiB payload；
整个 supervisor 1024 条、16 MiB payload。`mailbox.capacity` 接受 `application_id`，
返回 `application` 和 `supervisor` 两组 `messages/payload_bytes/max_messages/max_payload_bytes`。
字节数按压缩空白并转义 HTML 字符后的持久 JSON 表示计算，保证重启前后计费一致。
外部 send/timer 的原始 JSON 输入上限仍为 256 KiB；已接纳 timer 的 JSON 即使因持久化
转义而膨胀，恢复后仍可按实际编码大小入队。
这些限制只针对未确认工作，不包括仍保留的 acked/expired 历史、结果、commit 和审计，
不能据此宣称总内存或磁盘有界。

新 `message.send` 满载返回 RPC `-32005`，连接保持可用；客户端等待 ack 释放容量后使用
相同 key/payload 重试。已有记录的幂等重试优先于容量检查；规范化后的 JSON 字节不同仍返回冲突。
claim 不释放额度，ack 或 runner 完成成功持久化后才释放；失败回滚不释放。retire 将
pending/claimed 标为 expired 并释放执行额度。旧数据库高于新限额时保留原记录，允许
处理和确认，直到降至限额以下才接受新消息。

LSFS 满载时保存最后成功投递的 cursor，不越过受阻 artifact；timer 满载时保留原 due
及事件 identity。两者都不会因容量不足终止 runner，重启和释放容量后继续投递。周期
timer 保持原有合并语义：只补投一个到期事件，成功接纳后下一 due 为当前时间加 interval，
不为所有错过的 tick 扩展事件。

`route.*` 只能查看或更新 Application 版本允许的 route policy，不能直接指定未授权
backend。`hostfs.*` 返回 mount capability 的摘要和事件游标；宿主路径必须由 host bridge
再次校验。`artifact.*` 使用内容 hash 和幂等键，传输中断可以安全重试。`message.*` 只
操作 gateway 的 channel、订阅和 outbox，实际 Agent 唤醒由 durable mailbox 与 wake policy
决定。

## Permission broker

待确认请求结构：

```json
{
  "request_id":"req-42",
  "aid":1234,
  "turn_id":"turn-7",
  "operation":"tool:web.fetch",
  "scope":{"url_prefix":"https://example.com/"},
  "taint_summary":["external-content"],
  "ttl_seconds":60
}
```

CLI/GUI 展示请求并提交 approve/deny。批准不会直接发放任意权限；kernel 根据父 capability
和策略生成收窄后的临时 capability，并记录 grant、use、revoke 或 deny。没有可用的 client
时，CLI 仍可完成同一流程；ACP adapter 只是把请求映射为 `session/request_permission`。

没有 CLI、GUI 或 ACP client 连接时，Application 仍按自身 `wake_policy` 处理 durable
mailbox、进入 quiescent 或被 supervisor 唤醒。客户端断开不产生 Application retire 事件。
