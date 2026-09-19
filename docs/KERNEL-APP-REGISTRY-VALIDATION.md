# Kernel Application Registry 验证

2026-09-19，本轮加入 `0010-aok-application-registry.patch`，补齐 P1 事件链路的四项
缺口：内核 durable replay、LSFS source、Application registry 与 supervisor 接线。
验证使用固定 Linux baseline `f6388029ea9e2c9e807d73827658738ea131faee`、Debian arm64
builder（Apple Container `aok-p1-builder`）和本机 QEMU `virt` arm64。

## 实现边界

- `aok_application_create` 返回 application fd，把非零 `application_id` 注册进全局
  registry；同一 id 的再次 create 是幂等重开（新游标），这是 supervisor 重启后的
  恢复入口。创建要求 parent 持有 `MANAGE_CHILD`（root 或受管 aproc）。
- registry 条目在本次 boot 内永不释放：registry 自持引用，未确认事件跨 source fd
  释放与进程退出存活；挂接同一 application 的新 source 或 application fd 自身都可
  replay。每个条目约 8 KiB 预分配节点，数量上界是本次 boot 注册的 application 数。
- durable 队列深 128：timer 到期并入队；port/LSFS 的 `AOK_EVENT_POST` 满载返回
  `EAGAIN` 且不消耗 event id/sequence，由发送方在 ack 释放容量后重试；timer 合并
  计数经 flush 以单条 coalesced 事件补投。
- `AOK_EVENT_ATTACH_APP` 把 timer/port/LSFS source 挂到 registry：事件进入
  application 队列（全局 id/sequence 空间），记录携带触发 source 的 koid（合并与
  回注事件为 0）。挂接要求 source `WRITE` 与 app fd `READ|WRITE`，且 volatile
  积压未清空时拒绝挂接。detach 要求 `application_id==0` 并清除标签。
- ack 按 application 隔离：`aok_event_ack` 只作用于本 source 挂接的 application
  队列；`AOK_APP_ACK`（application fd ioctl）直接确认本队列。
- `aok_application_snapshot`（`INSPECT`）拷出整个未确认队列，reserved 字段清零，
  可原样过 `aok_application_restore`（`WRITE`）；restore 要求空队列、id 匹配且
  record 单调递增，并把 id/sequence 计数器推进到 max(现有, 回注) 之后——boot 内
  id 永不复用。
- 挂接状态下的换绑在读取时惰性重置游标：per-handle 记录游标所属队列，跨队列
  重放从新队列头部开始，不会跳过低序号事件。
- `aok_object_inspect` 支持 application fd（`state` 为未确认条数）。

## QEMU 结果

启用 `CONFIG_AOK_EXPERIMENTAL=y` 的最终镜像（`kernel/.build/qemu-arm64-p10`，
manifest 哈希齐全）：

```text
appregistry 45/45
eventwake    37/37
eventsrc     38/38
object       44/44
task         64/64
resource     53/53
core         46/46
```

禁用构建的 ENOSYS 回归：

```text
appregistry-disabled 5/5
eventsrc-disabled     6/6
resource-disabled     8/8
task-disabled         11/11
object-disabled       4/4
```

`appregistry`（`kernel/kselftest/appregistry.c`）覆盖：注册/幂等重开/权限与参数
拒绝、LSFS post 携带 cursor、跨 source 共享 id/sequence 空间、事件跨 source fd
释放与跨进程（fork 子进程投递）replay、app fd 与 duplicate 的 fresh cursor、
`AOK_APP_ACK`/source ack、ack 按 application 隔离、READ/WRITE/poll 权限矩阵、
满载 `EAGAIN` 不消耗 identity、snapshot 计数/有序拷贝/snapshot→restore round-trip、
restore 拒绝非空队列/异属 id/非单调/超量、restore 延续 id 空间、timer 到期在
source 关闭后仍 durable、volatile 积压阻塞挂接、挂接后不可换标签、换绑游标
重置、detach 清除标签。

反向 mutation：临时删除 `aok_event_read` 的游标重置检查后重建，
`appregistry` 第 44 项（rebound source replays from cursor reset）失败，其余 44 项
通过；恢复源码重建后 45 项全过。

本轮同时修复两个既有问题：

- `kernel/kselftest/resource.c` 第 53 项断言刚 fork、尚未被调度的子任务 RSS>0，
  属时序假设错误（在 0009 时代内核上即 2/4 偶发失败，此前记录的 53/53 为运气）。
  观测前加入 150 ms 稳定等待后，新旧内核均 4/4+ 通过。
- 根目录 `Makefile` 的 object/task/resource/eventsrc/core 测试 target 此前硬编码
  旧输出目录，`AOK_OBJECT_OUTPUT` 不生效；已统一改为可覆盖。

## Supervisor 接线

- `runtime/kernelbridge` 新增 0010 ABI 绑定（仅 linux/arm64；其余平台返回
  `ErrUnsupported`）：`Registry.OpenApplication`/`OpenEventSource`、`Attach`、
  `PostLSFS`、`Read`/`Ack`/`Snapshot`/`Restore`，结构体大小有编译期 layout 断言。
- `Supervisor.SetKernelBridge` 注入 `KernelEventBridge` 接口；Application 获得
  持久 `kernel_id`（单调分配，重启保留），`kernel:` 为保留幂等键前缀。
- drain 协议：先以 `kernel:<app>:<event_id>` 幂等键写入 durable mailbox 并提交，
  再 ack 内核事件；mailbox 满载时丢弃 handle（下次以新游标从头重放，幂等键去重
  已投递前缀），不 ack、不丢失。
- 关停时按需打开 handle 并把未确认队列 snapshot 进 `kernel_pending`；下次启动
  restore 回内核（跨 VM 恢复）。同 boot 重启时内核队列仍存活，restore 被拒后
  丢弃持久副本，由同一幂等键去重。retire 关闭 handle 并清除持久副本。

runtime 侧验证（本机 macOS）：

```sh
cd runtime
go test -race ./...
go vet ./...
GOOS=linux GOARCH=arm64 go build ./...
GOOS=linux GOARCH=arm64 go vet ./kernelbridge/
python3 scripts/smoke.py
python3 scripts/core-smoke.py
```

全部通过。`runtime/kernelwake_test.go` 用 fake bridge 覆盖：drain+ack、重复 drain
去重、满载保留事件与容量释放后续投、关停 snapshot→新 boot restore、同 boot
重启丢弃副本且不重复、幂等键保留、kernel_id 分配与持久化。

## 尚未完成

- 内核 registry 是 boot 内 durable：跨 VM 的持久性由 supervisor snapshot/restore
  承接，内核自身不落盘。fsd 尚未真实接入 LSFS source；`0009` 的 wake target
  仍是 source 私有，dormant aproc 创建与 `ON_QUIESCENT` 语义仍不完整。
- 按持久 owner/agent 隔离 application（registry 授权来自 parent capability）属于
  supervisor 策略层，内核未实现。
- 跨层背压协议、KASAN/lockdep 证据、compat handler 与非 arm64 ABI 仍未覆盖。
