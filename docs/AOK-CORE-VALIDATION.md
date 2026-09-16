# AOK P1-P3 核心交付证据

截至 2026-09-16，本轮把 P1-P3 中已经能在本地闭环验证的部分合并到同一套运行时：

- 内核 patch `0001`-`0008` 已加入 `kernel/patches/series`。`0006` 提供 root bootstrap、可收窄和可撤销的 capability、client/backend session、共享 token 预留、完成记账、取消和 export gate；`0007` 让 token hard limit 超限后保守记账并冻结 aproc；`0008` 将 budget snapshot 观察到的 CPU/RSS 超限转换为 fail-closed 冻结。
- 最新独立 P8 资源测试在同一 patch series 上通过 53/53，disabled 配置通过 8/8；独立回归重新通过 object 44/44、task 64/64、event-source 37/37、core 46/46。
- 同一个 kernel fd ABI 已由 `runtime/kernelbridge` 提供 typed Go binding。QEMU 通过 slirp 连接真实 llama.cpp CPU 服务，实测 input=11、output=32、used=43；预算超限返回 `EDQUOT`，祖先 revoke 后 descendant client/result 被拒绝。
- supervisor 使用 SQLite WAL/FULL 保存 application、durable mailbox、prepared result、checkpoint、timer、audit hash chain 和 context CAS。`python3 runtime/scripts/core-smoke.py` 通过 crash replay、headless timer 和预算冻结。
- engine 可由 `ProcessProvider` 以独立子进程运行，继承 listener fd，限制启动/请求时限、重启强度、输出大小，并支持 Linux sandbox launcher。崩溃、取消、启动超时、并发调用和真实 echo binary 均有测试覆盖。

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
