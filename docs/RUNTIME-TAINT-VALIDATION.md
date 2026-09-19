# Runtime 污点网关验证

2026-09-19，P3 首切片：`PLAN-L0` 3.5 与 `PLAN-AOK-DEEP` P3 验收项"污点注入被拦或
降级为确认"的 supervisor 侧实现。

## 实现

- 消息 payload 可声明 `taint` 位掩码（预定义 `TaintExternal`/`TaintConfidential`）；
  runner 在 claim 时把被消费消息的污点 OR 进 Application 的持久 `taint_bits`
  台账——此后该应用的输出继承污点标签（数据流标记，不是来源审计）。
- manifest 顶层 `export_mask`（默认 0）声明允许外发的污点位。控制面新增
  `application.export`：`taint_bits & ^export_mask != 0` 时拒绝（RPC `-32006`，
  `ErrTaintBlocked`）；调用方显式 `confirm: true` 时降级为已审计的人工确认放行
  （`taint.export.confirm` 审计记录本身就是确认凭证）。每次门控决策（allow/deny/
  confirm）都进 hash chain 审计。
- 与内核侧的关系：0006 的 capability `export_mask` gate（export ioctl 拒绝
  `taint & ~mask`）已在 QEMU 验证；本切片补齐 supervisor 边界的数据流标记与外发
  门控。`KernelInferProvider` 通过 `LastTaint()` 把内核会话污点（0006 的
  `io.taint |= session taint`）回流到 Application 台账——runner 在每个成功 turn
  后折算，外发门控同时看到 payload 声明与内核标记两类来源
  （`TestProviderTaintFlowsToExportGate` 以假 provider 覆盖；linux/arm64 的真实
  回流随 KernelInferProvider 由 event probe 间接覆盖）。

## 证据

```sh
cd runtime
go test -race ./...   # 含 TestTaintGateBlocksAndConfirms / TestTaintGateHonorsExportMask /
                      #     TestControlServerExportsTaintGate（真实 control socket RPC）
go vet ./...
GOOS=linux GOARCH=arm64 go build ./... && GOOS=linux GOARCH=arm64 go vet .
python3 scripts/core-smoke.py
```

全部通过。单测覆盖：干净导出放行、污点累计（3 = 两位）、默认 mask 拒绝、确认降级
+ 双审计记录、mask 位选择性放行（external 放行/confidential 拦截）、重启后台账与
门控保持、真实 RPC 的 -32006 与 confirm 放行。

## 尚未完成

- 两级撤销已完成（见 [RUNTIME-REVOCATION-VALIDATION.md](RUNTIME-REVOCATION-VALIDATION.md)）。
  人工确认慢路径已实现 runtime 侧（`escalate` 产生 pending 确认、
  `confirmation.settle` 审批、approved 请求单次放行、TTL 5 分钟、重启保留，
  `confirmation_test.go` 全覆盖，且审批与升级时的 payload 绑定、pending 队列
  上限 64+终局清扫、`message.claim` 控制面路径的污点折叠均由独立审查补齐并钉死；
  内核 seccomp unotify 监听器与宿主确认 UI 属
  后续。Sigsum/witness 联签、全系统 LSM 仍属后续。
