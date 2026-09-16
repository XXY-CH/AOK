# AOK 当前边界

这份记录把准备阶段容易混淆的名称、路径和实现状态固定下来。它与设计决议一起使用，
不替代 `docs/PLAN-AOK-DEEP.md` 的阶段计划。

## 已冻结

- **AOS** 是运行 AOK 的 SubOS 产品和 VM 环境名称；**AOK** 是 Agent-native kernel 和
  原生控制 CLI 的名称。Swift 可执行文件统一为 `aok`。
- `kernel/linux` 是上游 `linux-6.18.y` 的 detached shallow checkout，提交历史不与
  AOK 合并。macOS 使用大小写敏感卷承载它，根目录只保留 patch/config/kselftest。
- Agent 的产品 ABI 是 AOK fd/handle ABI。CLI、GUI、ACP、MCP、A2A 和 Linux/POSIX 用户态
  都是外部或用户态适配器；它们不能成为 Application 存活的条件。
- HostFS 的标准 guest 路径由挂载 capability 决定，示例使用 `/mnt/project/in` 和
  `/mnt/project/out`。路径字符串本身不授予权限。

## 当前已实现

- 设计文档、P0 ABI 冻结记录、Linux 基线拉取脚本、QEMU arm64 配置探针、patch/config/kselftest 准备目录。
- `aok up/down/ls/exec` 仅用于开发 VM 生命周期和探针；它还没有连接 AOK control API。
- stock baseline 已在 Debian arm64 builder 构建：Linux `6.18.51`，提交号由 `linux-6.18.y`
  shallow checkout 固定，`CONFIG_SCHED_CLASS_EXT=y`、Landlock、virtiofs、virtio-vsock、KVM
  和 initrd 配置均为 `y`。
- QEMU `11.1.1` arm64 `virt` 启动已验证：`/init` 执行成功，串口证据保存于
  `kernel/.build/qemu-arm64-stock/serial.log`，其中包含 `AOK_PROBE_STATUS=pass`。
- 可重复入口为 `make qemu-initramfs` 和 `make qemu-boot`；启动脚本会把探针行和错误模式写入串口日志。
- 第一枚 AOK 对象补丁已在 QEMU arm64 验证：空 aproc/AID、anon-inode fd、inspect、
  权限收窄和引用释放。41 项 kselftest 通过；禁用配置的 3 项 `ENOSYS` 测试通过。
  原型边界见 [abi/p1-object-prototype.md](abi/p1-object-prototype.md)。
- `0002` 已验证 PID1 root capability bootstrap；`0003` 已验证 aproc task/pidfd 生命周期，
  包括继承、attach、freeze/resume、abort、reap 和有序事件环。四组 enabled/disabled
  QEMU 测试及三枚 patch series 重建均通过，证据见 `kernel/.build/p3-report.md`。
- `0004` 已实现资源预算收窄、CPU/RSS 观测、token 台账和资源状态事件；`0007` 补充
  token hard limit 超限后的保守记账和冻结，`0008` 补充 budget snapshot 观察到 CPU/RSS
  超限后的 fail-closed 冻结。P8 资源测试记录 53 项启用通过；此前 disabled 8 项及
  object 44、task 64、event-source 37、core 46 回归保持通过。**CPU/RSS 仍不是实时
  调度器 throttle 或 memcg reclaim**。证据见 [KERNEL-P7-VALIDATION.md](KERNEL-P7-VALIDATION.md)
  和 [KERNEL-P8-VALIDATION.md](KERNEL-P8-VALIDATION.md)。
- `0005` 已实现 timer 事件源、ack、句柄内存 replay、满队列 coalesce 与权限检查；
  报告记录 37 项启用 + 6 项禁用测试，以及五枚 series 重建和八组回归通过。
  **尚无 durable replay、port/LSFS 源、poll 通知或实际 wake 动作**；
  ack 尚未按 application_id 隔离。见 `kernel/.build/p5-report.md`。
- 用户态 `runtime` 已有 JSON-RPC 消息编解码、离线 Echo provider、多 session/turn、
  prompt replay、abort/close、事件序列和 session 独立 token 台账。
  2026-09-16 本轮验证 `go test -race ./...` 与 `go vet ./...` 通过；
  新增独立 `aok-engine-echo` 与 Unix socket transport，接收父进程创建的 listener fd，
  实现握手、异步 prompt/abort、事件投递、连接隔离与断连取消；独立进程 smoke test
  通过。仍没有真实模型、内核 ainf 连接或完整的 v1 suspend/resume 背压。
  本轮证据与限制见 [RUNTIME-TRANSPORT-VALIDATION.md](RUNTIME-TRANSPORT-VALIDATION.md)。
- `make aok-object-build` 在 Linux arm64 上从固定上游提交导出独立源码并应用 patch；
  `make aok-object-test` / `make aok-object-test-disabled` 在 QEMU 中验证启用/禁用两种内核。
- P1-P3 核心联合证据见 [AOK-CORE-VALIDATION.md](AOK-CORE-VALIDATION.md)。新增 `0006` 内核推理 capability、
  `runtime/kernelbridge` typed ioctl binding、真实 llama.cpp QEMU probe、SQLite durable supervisor、
  context CAS/checkpoint、ProcessProvider 子进程监督和 sandbox launcher。runtime 的 `go test -race ./...`
  与 `go vet ./...` 已通过。

## 当前未实现

- 完整资源强制执行、持久事件源与唤醒、sched_ext Agent 调度、amem、ainf 设备化运行、完整 acap
  强制、AOK supervisor PID1/initfs、LSFS、router、Web capability、HostFS bridge 和消息 gateway
  尚未实现。QEMU kernel probe 已以 PID1 验证 capability/真实推理，但它仍是验证程序，不能视作完整
  AOK supervisor。
- `kernel/linux` 保持干净的上游基线；AOK 代码位于外层 patch，构建时应用到独立源码目录。
- 本机 QEMU 构建没有 virtio-vsock device model，当前只完成内核配置检查，未完成 guest↔host
  vsock 心跳；该项转移到 Apple Container 或支持 vsock 的 Linux/QEMU runner。
- 本机已安装 QEMU 11.1.1，可运行 guest 测试；macOS 系统 GNU Make 为 3.81，内核编译和
  `make kernel-config-probe` 仍使用 Linux builder。
