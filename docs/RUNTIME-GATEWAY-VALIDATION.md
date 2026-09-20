# 消息网关验证

2026-09-19，P4 首切片：`PLAN-AOK-DEEP` P4 验收"消息进入 mailbox 后按 wake policy
唤醒 Agent……将带 receipt 的回复送回原 conversation；断线重连不重复投递"的
runtime 侧。

## 实现

按 `lsfs-schema.md` message_* 表的运行时快照形式（`channels`/`conversations`/
`outbox`，与既有 mailbox/audit 同一持久化事务）：

- **Channel/Conversation**：`message.channel.create/revoke` 注册持久通道；
  `message.conversation.bind` 把外部身份稳定映射到 application（同一身份不可改绑
  其他应用）。
- **入站**：`gateway.deliver(channel, external_id, sequence, payload)` 以
  `gw:<channel>:<id>:<seq>` 幂等键进入目标应用 durable mailbox，游标只在 mailbox
  接受后推进（回滚则重投同序列仍幂等）。唤醒交给既有 wake 机制：on_event 由
  runner 无客户端驱动 turn，manual 离线累积 pending。
- **出站**：`gateway.reply(message_id)` 把 turn 结果（含 checkpoint/receipt 字段）
  投回来源 conversation，`reply:<message_id>` 幂等；`gateway.outbox.claim` 按会话
  取 pending（at-least-once，重连可重复领取），远端按幂等键去重；
  `gateway.outbox.ack` 幂等确认并记录远端 receipt。outbox 上限 256 条、优先逐出
  终局条目。

2026-09-20 修复：重启从完整 `(channel, external_id)` 恢复来源，核验 application
绑定；拒绝冒号拼接键的结构碰撞。Reply 检查应用 serving、channel active 与
`TaintBits & ^ExportMask == 0`，拒绝写持久审计；ClaimOutbox 也拒绝撤销通道。
成功回复 external 入站需要 manifest 明确允许 `TaintExternal`，不能隐式放行。
回归覆盖同一通道四个会话重启、confidential 污点拒绝、deny 审计重启、撤销后禁止发出。

## 证据

```sh
cd runtime
go test -race ./...   # TestGatewayRoundTripWithReconnects / TestGatewayHeadlessWake
go vet ./... && GOOS=linux GOARCH=arm64 go build ./... && GOOS=linux GOARCH=arm64 go vet .
python3 scripts/core-smoke.py && python3 scripts/smoke.py
```

全部通过。往返测试覆盖：入站进 mailbox 并唤醒（真实 finishTurn 路径）→ 回复带
结果回原会话 → 重连三态（重复 deliver 幂等/claim 重领不丢/ack 幂等且确认后不再
领取）→ 序列门控与撤销拒绝 → 改绑拒绝。headless 测试覆盖 manual 离线累积与
重启后游标/幂等保持。

## 尚未完成

- 真实外部 transport（webhook/im connector 进程）、MCP/A2A/ACP 驱动、
  message_identities 与投递尝试表、TLS/签名验证、出站退避与死信。
- ABI 规定的 channel/conversation 范围 capability 与 Reply 人工确认升级尚未接入；
  当前未掩蔽污点直接拒绝，不能用此切片宣称完整出站授权契约已完成。
