# Runtime Unix transport 验证记录

日期：2026-09-16。范围为用户态 engine；本轮没有修改内核补丁或重新运行 QEMU。

## 已实现

- `runtime/server.go` 接收 supervisor 已创建的 Unix stream listener；每条连接拥有
  独立 Engine/provider/session，握手返回实际 listener 地址。
- `runtime/cmd/aok-engine-echo` 从继承 fd 启动，SIGINT/SIGTERM 取消活跃连接并退出。
  engine 不按客户端路径创建 socket，不把路径作为 capability。
- prompt 异步执行，但启动顺序与接收顺序一致；紧随其后的 abort 能取消同一 turn。
  单 writer 按序投递事件和响应，notification 无响应。
- 断连取消 provider；有界队列溢出与写超时关闭连接。连接、并发 prompt 和输入帧均有上限。
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

独立进程 smoke 使用 Python 创建 0700 临时目录与 0600 Unix socket，构建并启动
`aok-engine-echo`，传递 listener fd，检查握手、两轮完整事件与响应、close/health、
SIGTERM 正常退出以及 stdout 未混入协议数据。临时二进制、socket 与目录均自动清理。
取消测试使用可阻塞且遵守 context 的测试 provider；Echo 不构成真实模型取消验证。

## 尚未实现

- 真实推理 backend、内核 ainf、PID1 supervisor、capability 认证和 host control API。
- streaming provider、session suspend/resume、持久 replay；队列溢出关闭连接是原型
  失败策略，不能视为已完成规范要求的背压与恢复协议。
- 连接内 session/request 历史回收与总内存上限；当前只限制 transport 队列和并发。
- 不合作 provider 的强制终止；provider 必须遵守 context cancellation。

下一步应先完成 session 历史回收、明确队列流控/恢复语义，再把真实 backend 接到
现有 Provider 边界；本切片不代表 Engine Protocol v1 已完整验收。
