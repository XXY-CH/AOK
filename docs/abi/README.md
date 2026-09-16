# AOK ABI v1 Draft

状态：P0 设计初稿；P1 已有实验 syscall 编号并通过 QEMU 验证，但尚未承诺稳定兼容性。

本文档冻结 AOK 与 AOK runtime、引擎、acapd 和外部 control client 之间的
语义边界。实现必须先通过能力探测，再使用可选扩展；未知扩展必须返回
`AOK_ENOTSUP`，不能静默模拟更弱的语义。

## 分层

```
CLI / GUI / ACP adapter
          |
    AOK control API
          |
 AOK runtime / libOS / supervisor
          |  aok_* fd ABI
 Linux AOK kernel objects
          |  vsock / UDS / device backend
   engine, fsd, acapd, inference backend
```

AOK 的默认产品 ABI 是 Agent-native fd 对象模型，不要求通用 Linux 用户态或 POSIX 程序存在。
Linux task 仍可作为 aproc 的执行载体，但不构成 Agent 身份、权限或生命周期契约。
现有 `spawn/wait/signal/ps` JSON-RPC socket 仅作为可选的迁移适配器，不是 AOK fd ABI，也不是
ACP wire protocol。兼容映射见 [syscalls.md](syscalls.md)。

## 版本与标识

- ABI 名称：`aok-abi`。
- 当前版本：`1.0-draft`。
- AOK 对象的 `koid` 是 kernel 生成的 64 位值，在一次 boot 生命周期内不复用。
- `AID` 等于 aproc 对象的 koid；它不是 Linux PID、pidfd、ACP `sessionId` 或 turn id。
- ACP `sessionId`、engine handle id 和 LSFS object hash 都在各自命名空间内有效，不能
  互相替代。
- 结构体使用固定宽度整数、显式 `size` 和 `flags`；新增字段只能追加，旧实现必须依据
  `size` 忽略未知尾部。

## 必须先完成的实现探针

P0 规格可以先冻结语义。当前 fork 基线是 `linux-6.18.y`；以下项目必须在实现前由 probe 写出事实结果：目标基线是否包含
`sched_ext` 和 Landlock ABI 6、QEMU arm64 是否支持所需 vsock、目标 llama.cpp 是否提供
slot save/restore、后端是否能回报 usage、SQLite 构建是否提供 FTS5/vec。探针失败时必须
选择文档中定义的降级路径，而不是改变 wire 语义。

## 文档

- [object-model.md](object-model.md)：对象、句柄、rights、aproc、job 和事件。
- [syscalls.md](syscalls.md)：AOK fd syscall、资源记账、freeze 和兼容映射。
- [engine-protocol.md](engine-protocol.md)：引擎 JSON-RPC over UDS v1。
- [acap-schema.md](acap-schema.md)：Biscuit、Cedar、污点和 OS 强制的边界。
- [lsfs-schema.md](lsfs-schema.md)：LSFS v1 的 SQLite/git 语义数据模型。
- [acp-profile.md](acp-profile.md)：ACP v1 的 client-facing adapter profile。
- [control-api.md](control-api.md)：CLI、GUI、principal 和 permission broker。
- [application-model.md](application-model.md)：面向 Agent 的 Application 身份、契约和生命周期。
- [context-model.md](context-model.md)：上下文段、缓存层、COW、pagein/evict、工具产物和恢复语义。
- [route-model.md](route-model.md)：动态模型路由、资源预留、fallback 和 KV 兼容性。
- [web-application.md](web-application.md)：分层 Web capability、session、browser 和证据产物。
- [hostfs-model.md](hostfs-model.md)：virtiofs live mount、artifact 双向交换和冲突语义。
- [message-gateway.md](message-gateway.md)：人类消息 channel、durable inbox/outbox 和投递语义。
- [p0-freeze.md](p0-freeze.md)：错误、pidfd 绑定、事件背压、资源压力和 token 对账的冻结决议。
- [p1-object-prototype.md](p1-object-prototype.md)：第一枚实验补丁的实际 ABI、测试和未实现边界。
