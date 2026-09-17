# Engine Protocol v1

引擎是独立长驻进程，使用 newline-delimited JSON-RPC 2.0 over Unix domain socket。协议
只描述 runtime 与 engine 的边界；它不是 AOK syscall ABI，也不是 ACP wire protocol。ACP
adapter 可以把 ACP 的 session/prompt/update/cancel 映射到本协议，但两个协议的 envelope、
版本协商和 capability 集合分别冻结，不能复用同一个 socket 作为 wire 兼容的捷径。

## Handshake

engine 由约定名称 `aok-engine-<name>` 启动，并在首条 response 返回：

```json
{"jsonrpc":"2.0","id":"hello-1","result":{
  "aok_version":1,
  "engine":"echo",
  "capabilities":["stream","cancel","usage"],
  "address":"unix:///run/aok/engine/echo.sock"
}}
```

socket 必须由 supervisor 创建并限制权限。引擎不能通过环境变量、路径或 socket 名称自行
获得 capability；句柄引用使用连接作用域的 opaque id，例如
`{"$acap":"sess-42/term-7"}`，不能接受任意路径作为权限证明。

## Methods

必选方法：`initialize`、`session/new`、`session/prompt`、`session/abort`、`session/close`、
`health`。可选方法包括 `session/suspend`、`session/resume`、`session/usage`。
当前实现支持前两者；`session/usage` 目前仅作为 notification 发送，尚无同名查询方法。

`session/prompt` 是单个 turn。响应或终态 event 必须包含 `stop_reason`：`completed`、
`cancelled`、`failed`、`budget_exhausted` 或 `backend_unavailable`。

一个 engine session 可以承载多个 turn；`session_id` 在 engine 连接范围内唯一，`turn_id`
在 session 内唯一。ACP `sessionId` 不直接作为 engine `session_id` 使用，由 runtime 保存
显式映射。

## Event ordering and cancellation

事件使用 notification：

```text
session/started -> session/chunk* -> session/usage? -> session/done|session/failed
```

每个事件带 `session_id`、`turn_id` 和单调 `event_seq`。同一 turn 内不能重排或重复终态。
`session/abort` 幂等；收到后引擎停止可取消的模型/工具调用，先发送必要的 pending 状态，
再发送 `done(stop_reason=cancelled)`。不可中断的 backend 必须声明能力，runtime 只能在
turn 边界结束。

v1 的目标契约不把隐式 socket backpressure 当作协议语义：事件队列达到上限时，runtime
发送 `session/suspend`，引擎应停止产生新 chunk，恢复后继续同一 event sequence。

当前实现把出站队列溢出当作背压而非致命错误：事件投递返回是否被接收，被拒的事件保留在
进程内的有界尾部并把该 session 置为 suspended，`session/resume` 按原 `event_seq` 补发，
尾部过期时返回明确错误。响应没有 `event_seq`、无法补发，溢出仍然关闭连接。

因此队列上限已具备暂停与恢复语义，但 runtime 只停止投递，provider 仍可能继续生成，
背压尚未反向传播到 backend，也不提供跨进程崩溃的持久事件恢复。

崩溃后的协议外 `cleanup`、`health` 检查与持久 session replay 属于恢复目标；进程内
`session/resume` 的测试不能作为这条恢复链路已完成的证据。

## Evolution

JSON 字段必须可忽略未知字段；结构化协议可演进到 proto/ttrpc，但必须保持方法、错误、
event ordering 和 cancellation 语义一致。Cap'n Proto 只作为远期原生 capability wire，
不是 v1 依赖。
