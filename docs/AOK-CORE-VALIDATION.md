# AOK P1-P3 核心交付证据

截至 2026-09-16，本轮把 P1-P3 中已经能在本地闭环验证的部分合并到同一套运行时：

- 内核 patch `0001`-`0008` 已加入 `kernel/patches/series`。`0006` 提供 root bootstrap、可收窄和可撤销的 capability、client/backend session、共享 token 预留、完成记账、取消和 export gate；`0007` 让 token hard limit 超限后保守记账并冻结 aproc；`0008` 将 budget snapshot 观察到的 CPU/RSS 超限转换为 fail-closed 冻结。
- 最新独立 P8 资源测试在同一 patch series 上通过 53/53，disabled 配置通过 8/8；独立回归重新通过 object 44/44、task 64/64、event-source 37/37、core 46/46。
- 同一个 kernel fd ABI 已由 `runtime/kernelbridge` 提供 typed Go binding。QEMU 通过 slirp 连接真实 llama.cpp CPU 服务，实测 input=11、output=32、used=43；预算超限返回 `EDQUOT`，祖先 revoke 后 descendant client/result 被拒绝。
- supervisor 使用 SQLite WAL/FULL 保存 application、durable mailbox、prepared result、checkpoint、timer、audit hash chain 和 context CAS。`python3 runtime/scripts/core-smoke.py` 通过 crash replay、headless timer 和预算冻结。
- runner 在 provider 取消或结果提交失败时会把 `claimed` 消息原子地 requeue 为 `pending`，因此同一 supervisor 进程停止后也能继续处理；`TestRunnerRequeuesClaimOnStop` 覆盖该路径。
- supervisor 重启时为每个未退役 Application 递增持久 `generation`，把旧 incarnation 的 `claimed` mailbox 重新置为 `pending`，并写入 `application.recover` 审计事件；worker 可在新 incarnation 继续处理同一 identity、context 和 checkpoint。
- engine protocol 已支持 `session/suspend`/`session/resume`：暂停期间保留有界事件尾部，恢复时按原 `event_seq` 补发；无法从保留尾部恢复时显式返回 `event history expired`。
- `aok-init`、`aok-supervisor` 和 manifest 已由 `make aok-initramfs` 打入可启动的 `newc` gzip archive，档案包含 `/init`、`/sbin/aok-supervisor`、`/etc/aok/manifest.yaml` 和 `/var/lib/aok`；当前缺少 kernel Image，因此尚未做 guest boot 验证。
- engine 可由 `ProcessProvider` 以独立子进程运行，继承 listener fd，限制启动/请求时限、重启强度、输出大小，并支持 Linux sandbox launcher。崩溃、取消、启动超时、并发调用和真实 echo binary 均有测试覆盖。
- 子进程 listener 地址按平台分离：Linux 用抽象命名空间，其他平台绑定子进程私有 0700 目录内的 `engine.sock`，parent 关闭前设置 `SetUnlinkOnClose(false)`，socket 只随 `reapLocked` 删除目录而消失。此前统一的 `@` 前缀在 macOS 上每次启动泄漏一个 socket 文件（实测累积 253 个），现由 `TestProcessProviderSocketStaysInPrivateDir` 守护。

验证命令：

```sh
cd runtime && go test ./... && go test -race ./... && go vet ./...
python3 runtime/scripts/core-smoke.py
```

## 尚未闭合的验收项

这些项目仍不能宣称已完成：

- `aok-init` 已成为可运行的 guest PID1 基础入口，负责 supervisor 子进程的信号转发、退出回收和 restart intensity；默认 supervisor 仍可在自身进程内调用 Provider，独立 `ProcessProvider` 路径已有测试。
- runtime 已提供签名可收窄 capability token、Cedar 风格 deny-overrides 和 manifest → Landlock/seccomp 强制；内核 capability export gate 仍不是全系统污点 LSM，unotify 和 external witness 尚未实现。
- runtime 已提供 llama slot save/restore、loopback Metal adapter、Anthropic Messages adapter、router/fallback、vsock API 和 cgroup v2 CPU/RSS enforcement。当前机器没有 `ANTHROPIC_API_KEY`，因此 Anthropic 真实服务验证被阻塞；当前 QEMU 也没有可用 virtio-vsock device model。
- kernel probe 的 backend adapter 与 PID1 同进程；尚未证明独立 backend 的长期 guest↔host vsock 心跳或 VM 重启后的真实模型 KV 恢复。
- QEMU arm64 的 PID1 probe 通过 46/46 新断言，并回归 object 44/44、task 64/64、resource 50/50、event-source 37/37。证据在 `kernel/.build/qemu-arm64-core/probe-serial.log` 和 `kernel/kselftest/CORE-VALIDATION.md`。


## 2026-09-19 追加：supervisor 推理经内核 ainf 设备闭环

`runtime/kernel_infer_linux_arm64.go` 新增 `KernelInferProvider`：supervisor 的每个
turn 都经 0006 推理 ABI 执行——prompt 作为 client session 提交（预留
input+声明输出上限），被包裹的 provider 只服务 backend 半边（Take→推理→
Complete 实报 usage），内核结算的 capability 台账与结果 payload 是唯一被接受的
outcome；预算与 EDQUOT 强制全部在内核。协议要点（均由 guest 实测钉死）：
session 序号从 1 起（跨 session 复用会 ESTALE）、Submit 预留/Complete 实报
（实报超预留即 EDQUOT）、`Result.TokensUsed` 是 capability 累计台账（对账按
provider 已结算累计）。

event probe 第三阶段（`make aok-event-probe-test`）在真实 AOK 内核上以 PID1 运行
真实 supervisor：短 turn 经内核会话完成并按内核对账（in=10/out=10），预算 128
下第二个超限 turn 在 Submit 处被内核拒绝、turn fail-closed：

```text
AOK_EVENT_PROBE_INFER=pass budget=128 used=20
AOK_EVENT_PROBE=pass
```

macOS/非 arm64 构建返回 `ErrUnsupported`，supervisor 回退直连 provider。

独立审核后补齐的失败路径账本与回归：Cancel（RUNNING 态）与失败的 Complete 都按
全额预留计费，provider 侧 `settled` 同步跟踪这些收费，Complete 全程持锁（capability
级台账在 Result 时是全局视图，并发会话会假性失配）。event probe 的回归场景：注入
一次 backend 失败（取消、按 68 token 全额计费）后，后续 turn 仍精确对账。
