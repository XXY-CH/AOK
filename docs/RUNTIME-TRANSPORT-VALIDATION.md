# Runtime Unix transport 验证记录

日期：2026-09-17。范围为用户态 engine；本轮没有修改内核补丁或重新运行 QEMU。

## 已实现

- `runtime/server.go` 接收 supervisor 已创建的 Unix stream listener；每条连接拥有
  独立 Engine/provider/session，握手返回实际 listener 地址。
- `runtime/cmd/aok-engine-echo` 从继承 fd 启动，SIGINT/SIGTERM 取消活跃连接并退出。
  engine 不按客户端路径创建 socket，不把路径作为 capability。
- prompt 异步执行，但启动顺序与接收顺序一致；紧随其后的 abort 能取消同一 turn。
- `session/suspend` 暂停后续事件投递但不丢弃有界事件尾部；`session/resume` 在尾部仍可用时按原 `event_seq` 补发，恢复失败会显式报错。
  单 writer 按序投递事件和响应，notification 无响应。
- 断连取消 provider；写超时关闭连接。连接、并发 prompt 和输入帧均有上限。
  出站队列溢出对事件不再是致命错误：`onEvent` 返回投递结果，被拒的事件仍保留在有界
  尾部并把 session 置为 suspended，客户端排空后用 `session/resume` 继续同一
  `event_seq`。响应没有序列号、无法补发，因此响应溢出仍然关闭连接。
- 每个 session 的历史上限会跨 session 相乘（64 × 2 MiB = 128 MiB/连接，32 条连接
  4 GiB），因此 Engine 另有 `maxEngineHistoryBytes`（16 MiB）约束总和：超限时从
  保留最多的 session 逐条淘汰最旧事件并推进其 `historyFloor`，`releaseSession`
  归还该 session 的配额。
- `ProcessProvider` 的子进程 listener 地址按平台选择：Linux 使用抽象命名空间，
  其他平台（Darwin/BSD）绑定到子进程私有 0700 目录内的 `engine.sock`。此前统一使用
  `@` 前缀，而只有 Linux 把它当抽象名；Go 又不会 unlink 以 `@` 开头的路径，因此
  macOS 上每次 engine 启动都会在工作目录泄漏一个 socket 文件（实测累积 253 个）。
  parent 关闭 listener 前设置 `SetUnlinkOnClose(false)`，socket 生命周期只由
  `reapLocked` 删除私有目录决定。`TestProcessProviderSocketStaysInPrivateDir`
  断言 listener 位于私有目录内、目录权限为 0700、reap 后目录消失且工作目录无新增条目。
- decoder 同时支持 request/response/notification，拒绝错误 envelope 和非法 ID，
  每次解码清空目标，避免复用对象残留前一个请求的字段。

## 实际验证

在 runtime 目录执行：

```sh
go test -race ./...
go vet ./...
python3 scripts/smoke.py
```

全部通过。smoke 输出：

```text
AOK_ENGINE_SMOKE=pass (inherited Unix listener, handshake, two turns, close, SIGTERM)
```

根目录快捷入口为 `make runtime-test` 和 `make runtime-smoke`。

Go 测试通过真实 Unix socket 覆盖握手门禁、连续两轮事件身份/序列、连接间 session
隔离、notification、关闭 session、流水发送 prompt/abort、畸形帧恢复、断连取消。
另用 `net.Pipe` 验证不读取响应的消费者触发发送队列上限后，连接能退出。
全部测试在 race detector 下运行。

socket 泄漏回归按 red/green 确认：把 `engineAddr` 临时改回 `@` 前缀后
`TestProcessProviderSocketStaysInPrivateDir` 失败，且该次运行确实在工作目录新增 1 个
socket；恢复修复后测试通过，两轮完整 `go test -race ./...` 与两个 smoke 之后工作目录
新增条目为 0。此前累积的 253 个泄漏 socket 已删除（仅按 socket 类型匹配，无其他文件受影响）。
修复同时通过 `go vet ./...` 与 `GOOS=linux GOARCH=arm64` 交叉编译/vet，覆盖两条平台分支。

独立进程 smoke 使用 Python 创建 0700 临时目录与 0600 Unix socket，构建并启动
`aok-engine-echo`，传递 listener fd，检查握手、两轮完整事件与响应、close/health、
SIGTERM 正常退出以及 stdout 未混入协议数据。临时二进制、socket 与目录均自动清理。
取消测试使用可阻塞且遵守 context 的测试 provider；Echo 不构成真实模型取消验证。

## 尚未实现

- 真实推理 backend、内核 ainf、PID1 supervisor、capability 认证和 host control API。
- streaming provider、持久跨进程 session replay。进程内溢出已走 suspend/resume，
  但 runtime 只是停止投递，provider 不会因此停止生成，也没有跨进程崩溃恢复。
- 不合作 provider 的强制终止；provider 必须遵守 context cancellation。

下一步应把真实 backend 接到现有 Provider 边界，并让背压反向传播到 provider；
本切片不代表 Engine Protocol v1 已完整验收。
