# 用户态 LSFS 持久唤醒验证

2026-09-18，本轮完成已有 context CAS/COW/checkpoint 上的 artifact commit 唤醒切片。
代码入口为 `runtime/wake.go`、`runtime/runner.go` 和 `runtime/control.go`。

## 已实现

- LSFS binding 按持久 Application ID 保存 source context、cursor 和 enabled 状态。
  控制面支持 create/list/disable/bind，以及幂等 JSON artifact.import；原 timer 调用保持兼容。
- artifact 先在 context SQLite 中提交，runner 扫描 commit log，将 mailbox 和 cursor
  一次提交到 supervisor SQLite。提交失败恢复内存中的 mailbox、cursor、sequence 和 audit；
  重启后重扫持久 commit，事件键去重。只有 artifact commit 投递，其他 commit 只推进 cursor。
- source 必须属于目标 Application 的 owner。每个 Application 最多 64 个 binding，
  每轮每个 binding 最多扫描 64 条 commit；手动投递不能占用 `lsfs:` 内部事件键。
- disabled binding 保留 cursor，重新启用时补扫；manual/frozen 只积累 pending，
  tombstoned Application 不再投递。runner 写回的 append/checkpoint 不形成自激循环。
- mailbox 准入同时限制未确认工作的条数和 payload 字节：每 Application 256 条/2 MiB，
  supervisor 合计 1024 条/16 MiB。pending/claimed 都计费，ack/runner 完成成功提交后才释放；
  retire 将未确认消息标为 expired。计费直接从持久状态推导，失败回滚和重启不依赖缓存计数。
- 满载 `message.send` 返回可重试 RPC `-32005`；LSFS 保留受阻事件之前的 cursor，timer
  保留原 due。容量不足不会退出 runner；释放容量后按原 identity 续投。`mailbox.capacity`
  可查看两级用量与上限。timer 保持原有错过 tick 合并语义，不扩展积压 tick。

## 验证证据

以下命令均在本地 macOS 执行通过，模型使用离线 Echo：

```sh
cd runtime
go test -race ./...
go vet ./...
GOOS=linux GOARCH=arm64 go build ./...
python3 scripts/smoke.py
python3 scripts/core-smoke.py
```

core smoke 真实启动 supervisor，提交 artifact 后 SIGKILL，重启后验证 disabled binding
和 cursor 恢复，再启用 source；无持续连接的客户端参与也能完成 turn/checkpoint。第二次
重启确认 mailbox identity 保留且没有 runner 写回产生的额外事件。输出中
`crash_replay/headless_timer/headless_lsfs/lsfs_restart` 均为 `true`。

`TestLSFSWakeRestartAndHeadlessCompletion` 覆盖扫描前/后重启、artifact 幂等重试、
headless 完成和写回过滤；`TestLSFSWakeAtomicRollback` 使用 SQLite trigger 注入提交失败，
检查完整状态回滚，并回退 cursor 验证重复扫描去重。其余测试覆盖生命周期、owner 隔离、
起始 cursor、批量边界、commit 顺序和真实 control socket 的 create/import/disable/bind/list。

两项 mutation 均已实际运行，并在恢复后重新验证：

| 临时破坏 | 失败证据 |
| --- | --- |
| 将 artifact 过滤改为接受全部非空 commit kind | `TestLSFSWakeRestartAndHeadlessCompletion` 失败，1 条原事件之外又出现 result/checkpoint 两条 pending 事件。 |
| 移除 `persistLocked` 失败时的 `restoreLocked` | `TestLSFSWakeAtomicRollback` 失败：`failed transaction changed mailbox, cursor, sequence or audit`。 |

后续 overflow/replay 切片同样通过上述全量检查。`TestMailboxCapacityLimitsAndRecovery`
分别填满单 Application/全局的条数/字节限制，验证幂等重试、claim 仍占额度、重启保留占用、
失败 ack 不释放、成功 ack 和 retire 释放容量。`TestLSFSWakeBackpressureReplay` 验证部分
批次投递后的 cursor、不影响其他 Application 执行、重启及释放额度后无重复续投。
`TestTimerBackpressureRestartAndRollback` 覆盖 one-shot/periodic 的保留 due、重启和写失败回滚。
真实 control socket 测试验证 `mailbox.capacity`、`-32005` 及同连接释放额度后重试。

本切片另有两项 mutation 验证：移除 supervisor 总量检查时，条数和字节两个子测试都以
`overflow admitted: <nil>` 失败；让 LSFS 在拒收后仍推进 cursor 时，以
`cursor passed an undelivered commit` 失败。所有临时破坏均已恢复。

JSON 表示还经过 red/green 验证：压缩空白和转义 `<>&` 使同一 payload 在重启前后从
20 bytes 变成 29 bytes。`TestMailboxCapacityStableJSONEncoding` 在修复前复现这一差异，
修复后计费和幂等重试均稳定。`TestTimerEscapedPayloadRecovery` 验证接纳的 256 KiB 原始
timer JSON 经转义膨胀后仍可在重启后投递，按实际编码字节计费。

## 仍未完成

这是用户态持久 artifact wake，不是完整 amem/LSFS 或内核事件 ABI。port、poll 通知、
内核实际 wake、跨 VM 恢复、LSFS GC/retention 和 aggregate disk quota 尚未实现。
新的 mailbox 上限仅约束未确认工作；acked/expired 历史、结果、commit 和 audit 仍保留，
不能视为总内存或磁盘上限。容量查询遍历现有 mailbox，尚无独立索引或历史归档。
runner 串行执行 provider，长 turn 会延迟扫描；64 条批次限制也不是全局扫描调度预算。
事件提供 artifact 元数据和文本通知，不自动读取 artifact 内容供模型推理。

本轮没有重跑 QEMU 或真实 llama.cpp；Linux arm64 仅交叉编译。Engine Protocol 的
跨进程 durable replay 和 provider-level backpressure 也仍未实现。artifact 与审计分别
写两个数据库，审计失败时 artifact 可已持久化，重试必须沿用原幂等键与 payload。
