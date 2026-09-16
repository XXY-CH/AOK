# ACP v1 Profile for AOK

ACP 是外部 Client 与 Agent runtime 的 session 协议。它不替代 AOK syscall、内部 channel
或 capability。profile 固定 ACP v1；实现必须记录采用的 schema commit，不跟随 upstream
`main` 漂移。ACP adapter 是唯一对外 client-facing session 边界；engine protocol 是
runtime 到后端进程的内部边界。

## Mapping

| ACP | AOK |
|---|---|
| `initialize` | 版本和 capability negotiation，不创建 aproc |
| `session/new` | 创建一个 session handle，并绑定一个 aproc（MVP 一对一）；返回 client-visible `sessionId`、绝对 `cwd` 和协商后的可选能力 |
| `session/prompt` | 创建一个 scheduler turn，内容写入 amem message pages |
| `session/update` | 有序 AOK event，经 mailbox/event stream 发出 |
| `session/cancel` | 取消当前 turn；级联取消可取消工具和 pending permission |
| `session/request_permission` | 映射到 permission broker；结果仍由 kernel 检查并审计 |
| `session/load` | 后续扩展；必须显式区分 replay 与 resume |

`sessionId` 是 client-visible opaque string；AID 由 kernel 生成，不能由 Client 指定。COW
fork 创建新的 aproc/session，不能让两个 session 共享可变 history。ACP filesystem、
terminal 和 MCP 字段只表示协议能力，不能直接授予 AOK capability。

`session/prompt` 的 `ContentBlock[]` 追加到该 session 的消息页并创建一个新的 `turn_id`；
同一 session 的 prompt 按序执行。`session/load` 的恢复语义是后续扩展，必须区分“回放完整
conversation”和“恢复而不回放”；在这两种语义冻结前不得把 load 映射成简单的 KV restore。

## Transport

基线 transport 是 stdio newline-delimited JSON-RPC。AOK 可提供 stdio、UDS 或 vsock adapter，
但每种 transport 都必须保持同一 ACP envelope、notification、双向 permission 和取消语义。
旧 syscall socket 不得直接复用为 ACP endpoint。

## Permission and cancel

权限请求至少携带 `request_id`、`session_id`、`aid`、`turn_id`、tool、scope、选项和 TTL。
客户端返回的 allow/reject/cancel 只是用户决定；kernel 仍执行 manifest、父 capability、
taint、资源和不可逆操作检查。`session/cancel` 是 notification，原 prompt response 最终
以 `stopReason=cancelled` 结束；不能表现为普通 engine error。

## Capability negotiation

未实现的 ACP capability 必须在 `initialize` 中省略或标为 unsupported。filesystem、terminal、
MCP、load/resume 等可选能力必须分别声明，不能用一个“agent supports ACP”总开关代替。
