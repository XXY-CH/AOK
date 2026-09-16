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

当前本地控制实现还提供只读的 `application.list`、`mailbox.list` 和
`event_source.list`，供 TUI 展示持久 Application、durable mailbox 和 timer 快照。

Application control 方法操作持久 `application_id`，再由 supervisor 选择或创建当前 aproc
incarnation。`aproc.*` 方法只操作当前 boot 的执行化身；两者不能互相替代。

`event_source.*` 方法创建或管理 timer、port 和 LSFS 变更源，并把它们绑定到持久
`application_id` 的 `wake_policy`。控制面断开不会删除绑定或清空 pending 事件；撤销绑定
必须经过 supervisor capability，并写入审计事件。

每个请求绑定 principal 和审计 correlation id。control API 只能使用 supervisor 委派的
管理 capability；越权返回 deny，不返回底层 root capability 的细节。

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
