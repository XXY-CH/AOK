# AOS — Agent-native SubOS

运行在 macOS（Apple Silicon）之上的 SubOS：每个 SubOS 实例是一个独立内核的 Linux microVM
（Apple Containers）。目标是通过 Linux AOK kernel fork 提供面向 LLM Agent 的资源域、上下文和
能力边界。AOK 的原生用户是 Agent runtime；现有 Go `agentd`、Linux/POSIX、CLI、GUI 和 ACP
都只是外部 runtime、观察控制或迁移适配器。

架构与 syscall 规范见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。

## 当前准备状态

```sh
# 获取上游 Linux stable 基线
make linux-fetch
# 如需手动重新挂载大小写敏感的本地内核卷
make linux-mount
# 查看基线版本和 AOK 准备目录
make linux-status
```

旧的 Go `agentd`、OCI rootfs 和 `/run/aos/syscall.sock` 已清除，不再提供旧的 spawn/ps
路径。stock 内核已启动成功，第一枚 AOK 对象补丁已通过 QEMU arm64 验证。

```sh
# 已有构建产物时，在本机 QEMU 重跑对象测试
make aok-object-test
make aok-object-test-disabled
```

Linux builder 的完整重建步骤见 [kernel/kselftest/README.md](kernel/kselftest/README.md)。
当前 series 包含对象、root capability、task/pidfd、资源记账、timer 事件源和 inference
capability、token hard freeze 和 observed resource enforcement 八枚补丁。
最新能力边界见 [docs/IMPLEMENTATION-STATUS.md](docs/IMPLEMENTATION-STATUS.md)。
候选补丁先执行 `make aok-candidate-check PATCH=...`，完整构建与回归通过后再整合。

用户态协议与独立 Unix socket engine 可独立验证：

```sh
make runtime-test
make runtime-smoke
python3 runtime/scripts/core-smoke.py
```

macOS 端交互入口：

```sh
swift run aok onboard             # 首次配置
swift run aok tui                 # TUI 控制台
swift run aok settings show       # 查看设置
swift run aok settings set engine echo
```

TUI 当前覆盖 SubOS 实例查看与启动、Supervisor health/Overview、Application 创建/查看/冻结/恢复、
token limit、durable mailbox/timer 快照、默认镜像、状态目录、control socket、aokctl、manifest
和引擎设置。TUI 通过 aokctl 调用 supervisor control API，不直接持有 kernel root capability。

## 状态

- [x] 设计与调研：Agent-native ABI、事件源、上下文效率、动态路由、Web、HostFS、消息网关
- [x] Linux fork 准备：`linux-6.18.y` 基线与 out-of-tree 目录
- [x] P0 ABI 语义冻结：错误、pidfd 绑定、事件背压、资源压力和 token 对账
- [x] stock kernel QEMU arm64 配置与启动探针
- [x] P1 第一枚实验对象补丁：AID、fd/inspect、rights 收窄；41 + 3 项 guest 测试
- [x] P1 root capability bootstrap：一次性 claim、`MANAGE_CHILD` 校验；43 + 4 项 guest 测试
- [x] task/pidfd 生命周期、资源记账、timer 事件源原型（限制见状态文档）
- [x] AOK P1-P3 可验证核心闭环：kernel inference capability、真实 llama.cpp、durable supervisor、context CAS、audit
- [x] Phase 3 基础能力强制：manifest → Landlock + seccomp sandbox launcher
- [ ] AOK P1 Agent 资源域强制执行（token hard freeze、观察超限冻结已完成；实时 throttle/reclaim 未完成）
- [ ] AOK supervisor PID1/initfs、amem/LSFS、channel IPC、sched_ext
- [ ] Phase 2 完整设备化：vsock、双后端、KV save/restore、router/fallback
- [ ] Phase 3 完整能力模型：Biscuit/Cedar、unotify、全系统污点和外部 witness
- [ ] Phase 4 多开并行调研（claude-engine + fan-out/aggregate）
- [ ] Phase 5 快照恢复 / eBPF 审计 / 调度器

## 规范

- [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)：分层和 Agent-native 边界
- [docs/abi/context-model.md](docs/abi/context-model.md)：上下文分页、缓存、COW 和恢复
- [docs/abi/application-model.md](docs/abi/application-model.md)：Application 持久身份和生命周期
- [docs/abi/route-model.md](docs/abi/route-model.md)：动态模型路由与 KV 兼容性
- [docs/abi/web-application.md](docs/abi/web-application.md)：分层 Web capability 与 browser fallback
- [docs/abi/hostfs-model.md](docs/abi/hostfs-model.md)：宿主 virtiofs 挂载与 artifact 交换
- [docs/abi/message-gateway.md](docs/abi/message-gateway.md)：人类消息网关与 durable mailbox
- [docs/IMPLEMENTATION-BASELINE.md](docs/IMPLEMENTATION-BASELINE.md)：内核基线、清理边界和启动顺序
- [docs/IMPLEMENTATION-STATUS.md](docs/IMPLEMENTATION-STATUS.md)：当前已冻结、已实现和未实现边界

## 结构

```
host/     Swift：未来的 aok CLI 与 VM/control API 适配器
kernel/   Linux AOK fork、AOK ABI、patch/config/kselftest 准备目录
manifests/ 能力清单示例
docs/     架构、决议和 ABI 规范
```
