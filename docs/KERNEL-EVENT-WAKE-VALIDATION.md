# Kernel 事件唤醒验证

2026-09-18，本轮在 `0008` 之后加入 `0009-aok-event-wake.patch`，把已有的 timer
event source 扩展为可轮询的 source，并增加有界 port source 与显式授权的 aproc 自动唤醒。
验证使用固定 Linux baseline `f6388029ea9e2c9e807d73827658738ea131faee`、Debian arm64
builder 和 QEMU `virt` arm64；`kernel/linux` 保持未修改。

## 实现边界

- source fd 要求 `READ` 才能 `poll`/读取；`WRITE` 才能配置目标和向 port 投递。
- port source 只保存 64 个未确认事件；满载返回 `EAGAIN`，不会消耗 event id 或 sequence。
- `AOK_EVENT_SET_TARGET` 要求 source 的 `WRITE` 和目标 aproc 的 `SIGNAL`，并 pin 目标
  file。关闭传入的目标 fd 不会撤销绑定；`aproc_fd=-1` 加 `MANUAL` 才会 detach。
- timer/port 通知会唤醒 source pollers，并异步恢复仍处于 `FROZEN` 的 aproc。自动唤醒
  不会绕过资源导致的冻结；显式、已授权的 resume 仍可用。`ON_EVENT` 与
  `ON_QUIESCENT` 在此切片都只处理现存 aproc，不创建 durable Application incarnation。
- 未确认事件会阻止换绑或重标记；关闭最后一个 source 引用会同步取消 timer/work 并释放
  目标引用。

## QEMU 结果

启用 `CONFIG_AOK_EXPERIMENTAL=y` 的最终镜像：

```text
eventwake 37/37
object    44/44
task      64/64
resource  53/53
eventsrc  38/38
core      46/46
```

`eventwake` 使用共享内存 heartbeat 子任务，实际验证 freeze 后 timer/port 恢复执行、
poll cursor、ack、权限、满载重试、MANUAL/detach、重复 50 次唤醒、资源冻结保护、
abort/reap 和目标 fd 关闭后的生命周期。

禁用 `CONFIG_AOK_EXPERIMENTAL=n` 后重新构建的镜像也通过现有回归：

```text
object    4/4   (ENOSYS)
task      11/11 (ENOSYS)
resource  8/8   (ENOSYS)
eventsrc  6/6   (ENOSYS)
```

主要命令为：

```sh
make aok-object-build
make aok-eventwake-test
make aok-object-test-disabled
make aok-task-test-disabled
make aok-resource-test-disabled
make aok-eventsrc-test-disabled
```

本轮还做了反向 mutation：临时移除自动唤醒中的 `resource_frozen` 检查，
`eventwake` 第 34、35 项随即失败；恢复源码并重建后 37 项全部通过。该证据说明测试
确实守护资源冻结不能被异步唤醒绕过，而不是只覆盖成功路径。

## 尚未完成

这是 native aproc 的 volatile kernel wake slice，不是完整 P1。内核还没有 durable
event replay、LSFS/amem source、Application registry 或 supervisor 接线；source 引用
释放后事件不会跨进程或跨 VM 恢复。`application_id` 是标签，授权仍来自 source/aproc
fd rights，尚未由独立 registry 认证。provider 持续生成和用户态 durable mailbox 仍由
runtime 自己处理；两者尚未形成跨层背压协议。实现只验证 arm64 native ABI，没有 compat
handler，也没有 KASAN/lockdep 证据。
