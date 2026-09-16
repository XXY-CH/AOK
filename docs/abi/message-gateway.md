# AOK Human Message Gateway ABI v1

消息网关是独立、持久化并由 supervisor 管理的 Application。Telegram、Slack、邮件、
Webhook、GUI 和 ACP 都是 channel adapter，不能直接调用 Agent 或拼接 system prompt。

```text
external channel -> adapter -> normalize/identity map -> durable inbox
                                                        -> Application mailbox
Agent outbound queue <- delivery worker <- channel adapter
```

## Normalized message

```json
{
  "message_id":"channel:abc-42",
  "channel":"telegram",
  "conversation_id":"tg:chat-7",
  "thread_id":"tg:thread-3",
  "external_principal":"telegram:user-9",
  "text":"请整理今天的研究结果",
  "attachments":[{"artifact":"sha256:...","mime":"application/pdf"}],
  "received_at":1770000000,
  "taint":["external-message"]
}
```

adapter 负责 webhook signature verification、身份映射、格式归一化、附件转 artifact、
message id 去重和 channel rate limit。网关负责 durable inbox/outbox、顺序、重试/backoff、
dead-letter、delivery receipt 和断线 replay。

## Agent 语义

消息先写 `application_mailbox`，由 Agent/Application 自己决定是否唤醒、创建 turn、路由
给另一个 Application、归档为 memory、请求人类确认或升级模型。没有在线 client 时，
mailbox 和 wake policy 仍然生效。

出站消息必须持有目标 channel capability，包含 conversation/thread 范围、格式、附件和
过期时间。`turn_id + request_id + idempotency_key` 防止重试重复发送；需要确认的副作用
进入 permission broker，拒绝、撤销和 delivery failure 都写审计链。

## 生命周期

channel、conversation、identity、inbox、outbox 和 delivery attempt 与 Application registry
在同一 LSFS 事务边界内创建或撤销。channel 断开不删除消息；重启后按游标 replay，已确认
的副作用不可再次执行。外部文本默认带 taint，只有策略明确允许时才能进入长期记忆或
跨 channel 外发。
