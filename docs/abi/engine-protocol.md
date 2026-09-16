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
`health`。可选方法：`session/suspend`、`session/resume`、`session/usage`。

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

v1 不把隐式 socket backpressure 当作协议语义。事件队列达到上限时 runtime 发送
`session/suspend`；引擎必须停止产生新 chunk，恢复后继续同一 event sequence。崩溃后
supervisor 运行协议外 `cleanup`，runtime 通过 `health` 和 session replay 决定是否恢复。

## Evolution

JSON 字段必须可忽略未知字段；结构化协议可演进到 proto/ttrpc，但必须保持方法、错误、
event ordering 和 cancellation 语义一致。Cap'n Proto 只作为远期原生 capability wire，
不是 v1 依赖。
