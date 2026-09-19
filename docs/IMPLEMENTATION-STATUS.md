# AOK 当前边界

这份记录把准备阶段容易混淆的名称、路径和实现状态固定下来。它与设计决议一起使用，
不替代 `docs/PLAN-AOK-DEEP.md` 的阶段计划。

## 已冻结

- **AOK** 是 Agent-native kernel、SubOS 产品和 VM 环境的统一名称，Swift 可执行文件统一为 `aok`。
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
- `0002` 已验证 PID1 root capability bootstrap（2026-09-19 修复多线程 PID1 的 tgid 判定）；`0003` 已验证 aproc task/pidfd 生命周期，
  包括继承、attach、freeze/resume、abort、reap 和有序事件环。四组 enabled/disabled
  QEMU 测试及三枚 patch series 重建均通过，证据见 `kernel/.build/p3-report.md`。
- `0004` 已实现资源预算收窄、CPU/RSS 观测、token 台账和资源状态事件；`0007` 补充
  token hard limit 超限后的保守记账和冻结，`0008` 补充 budget snapshot 观察到 CPU/RSS
  超限后的 fail-closed 冻结。P8 资源测试记录 53 项启用通过；此前 disabled 8 项及
  object 44、task 64、event-source 38、event-wake 37、core 46 回归保持通过。runtime 现在在 Linux
  cgroup v2 写入 `cpu.max`/`memory.max`，并由 `memory.current` 监控器触发 `memory.reclaim`；
  AOK 内核 patch 本身仍未提供专用 sched_ext quota 或独立 memcg。证据见
  [KERNEL-P7-VALIDATION.md](KERNEL-P7-VALIDATION.md) 和 [KERNEL-P8-VALIDATION.md](KERNEL-P8-VALIDATION.md)。
- `0005` 已实现 timer 事件源、ack、句柄内存 replay、满队列 coalesce 与权限检查；
  报告记录 37 项启用 + 6 项禁用测试，以及五枚 series 重建和八组回归通过。
  原始证据见 `kernel/.build/p5-report.md`。
- `0009` 新增 source fd 的 poll、有界 port 通知及 timer/port 恢复 frozen aproc。
  目标绑定必须持有 aproc `SIGNAL`；自动恢复不能绕过资源冻结，未确认事件阻止换绑。
  37 项实际 task 心跳/唤醒测试和 object 44、task 64、resource 56、event-source 38、
  core 46 项 QEMU 回归通过，见 [KERNEL-EVENT-WAKE-VALIDATION.md](KERNEL-EVENT-WAKE-VALIDATION.md)。
- `0010` 加入 Application registry、durable replay、LSFS source 与 supervisor 接线：
  application fd 按 `application_id` 幂等注册（boot 内存活），未确认事件跨 source fd
  释放与进程退出 replay；`AOK_EVENT_ATTACH_APP` 把 timer/port/LSFS 源挂入 128 深的
  durable 队列，LSFS post 携带 commit cursor；ack 按 application 隔离；
  snapshot/restore 交接 supervisor 实现跨 VM 恢复。appregistry 47 项 + 全部六套
  enabled（object 44、task 64、resource 56、event-source 38、event-wake 37、
  core 46）与五套 disabled ENOSYS 回归通过，含一项游标重置 mutation 验证，见
  [KERNEL-APP-REGISTRY-VALIDATION.md](KERNEL-APP-REGISTRY-VALIDATION.md)。
  runtime 侧 `kernelbridge` 绑定 0010 ABI，supervisor 以 `kernel:<app>:<event_id>`
  幂等键先持久后 ack 地 drain 内核队列，满载丢弃 handle 重放，关停 snapshot/启动
  restore。生产路径已通电：PID1 认领 root 经 `AOK_ROOT_FD` 传给 supervisor，LSFS
  binding 的 artifact commit 经内核 durable 队列路由（满载保留 cursor）。QEMU 端到端
  probe（`make aok-event-probe-test`）在真实内核上运行 ABI 阶段与完整 supervisor
  链路（binding→内核队列→drain→mailbox→echo turn→关停→重启精确一次续投）；
  `go test -race`、vet（含 linux/arm64 交叉）、smoke 与 core-smoke 通过。
  内核 registry 仍不落盘，跨 VM 持久性由 supervisor 承接；生产者是 supervisor 自身的
  LSFS 扫描器，独立 fsd 尚不存在；按持久 owner 的隔离属于 supervisor 策略层。
- `0011` 加入按资源 enforcement level（`cpu_level`/`memory_level`/`token_level`，
  WITHIN→THROTTLE→FREEZE；token 80%、观测 CPU/RSS 90% 进入 THROTTLE），结构尺寸
  不变，resource kselftest 扩到 56 项；runtime `claimTurn` 在同一压力带做准入节流
  （每应用每秒一个 turn）。同轮修复 runner 对容器挂载路径的陈旧页缓存问题：QEMU
  一律从本地临时副本引导。见 [KERNEL-P8-VALIDATION.md](KERNEL-P8-VALIDATION.md)。
- 2026-09-19 P2 切片：route_policy/route_record 对象落地——Application 持久化
  版本化 backend fallback 策略（router 按 context 过滤、直连 provider 调用前检查、
  越权 turn 以 `route_denied` 失败且不触达 backend），每个 turn 追加不可变 route
  record（policy 版本/backend/compat/fallbacks/cache_hit_kind/token 三项/理由码，
  supervisor 全局上限 512 条、新的逐出旧的），`application.set_route_policy` 与 `route.list` 上控制面。
- 2026-09-19 P2 切片：supervisor 推理经内核 ainf 设备闭环——`KernelInferProvider`
  把每个 turn 放入 0006 会话（预留/实报/对账/EDQUOT 全内核强制；Cancel/失败完成按
  全额预留计费并保持对账），event probe 在真实内核上验证短 turn 结算、取消后对账
  不发散与超预算 fail-closed，见
  [AOK-CORE-VALIDATION.md](AOK-CORE-VALIDATION.md) 追加节。生产 supervisor 的
  engine 开关尚未接入 kernel-infer，当前证据范围是 probe 内以真实 supervisor 栈
  运行。
- 2026-09-19 P2 切片：router 记录每 turn 的 provider/fallbacks/compat key 并按
  后端回报分类 cache_hit_kind（kv_exact/prefix_replay/text_replay）；调度带前缀
  亲和（公平带内优先共享前缀的 turn）。真实 llama 证明：无干扰命中 87/88，且在
  会逐出缓存的干扰 turn 排队在前时，亲和仍保住 87/88 命中。见
  [RUNTIME-ROUTE-AFFINITY-VALIDATION.md](RUNTIME-ROUTE-AFFINITY-VALIDATION.md)。
- 2026-09-19 P2 切片：前缀命中率按后端 usage 可复算——llama provider 强制
  `cache_n` 一致性，supervisor 累计 `tokens_cached` 并经 inspect/result 暴露；
  真实 llama.cpp smoke 实测命中率 0.00→0.99（87/88 前缀复用），见
  [RUNTIME-HITRATE-VALIDATION.md](RUNTIME-HITRATE-VALIDATION.md)。cache 亲和
  spawn 与 router KV compatibility 验证仍待做。
- 用户态 `runtime` 已有 JSON-RPC 消息编解码、离线 Echo provider、多 session/turn、
  prompt replay、abort/close、事件序列和 session 独立 token 台账。
  2026-09-16 本轮验证 `go test -race ./...` 与 `go vet ./...` 通过；
  新增独立 `aok-engine-echo` 与 Unix socket transport，接收父进程创建的 listener fd，
  实现握手、异步 prompt/abort、事件投递、连接隔离与断连取消；独立进程 smoke test
  通过。出站队列溢出已改为暂停 session 并可 `session/resume` 补发，Engine 另有
  跨 session 的历史总量上限；仍没有真实模型、内核 ainf 连接，背压也未反向传播到 provider。
  本轮证据与限制见 [RUNTIME-TRANSPORT-VALIDATION.md](RUNTIME-TRANSPORT-VALIDATION.md)。
- `make aok-object-build` 在 Linux arm64 上从固定上游提交导出独立源码并应用 patch；
  `make aok-object-test` / `make aok-object-test-disabled` 在 QEMU 中验证启用/禁用两种内核。
- P1-P3 核心联合证据见 [AOK-CORE-VALIDATION.md](AOK-CORE-VALIDATION.md)。新增 `0006` 内核推理 capability、
  `runtime/kernelbridge` typed ioctl binding、真实 llama.cpp QEMU probe、SQLite durable supervisor、
  context CAS/checkpoint、ProcessProvider 子进程监督和 sandbox launcher。runtime 的 `go test -race ./...`
  与 `go vet ./...` 已通过。
- supervisor 当前在重启时递增 Application `generation` 并恢复旧 incarnation 的 pending mailbox；engine
  transport 支持有界 `session/suspend`/`session/resume`。2026-09-19 起，完整 guest 全栈 PID1 boot
  （aok-init 认领 root 并经 `AOK_ROOT_FD` 传给 supervisor、内核 bridge 激活、echo 引擎就绪）已在
  AOK 内核 QEMU 中通过 `make aok-initfs-boot-test` 验收，见
  [GUEST-INITFS-BOOT-VALIDATION.md](GUEST-INITFS-BOOT-VALIDATION.md)；该切片同时修复
  `0002` root claim 对多线程 PID1 误判（`task_pid_nr` → `task_tgid_nr`）与 initfs
  状态目录权限。监督重启的 guest 内演练仍待做。
- `ProcessProvider` 子进程 listener 地址已按平台分离：Linux 抽象命名空间，其他平台绑定到子进程
  私有 0700 目录。此前在 macOS 上每次 engine 启动泄漏一个 socket 文件（累积 253 个），现由
  `TestProcessProviderSocketStaysInPrivateDir` 以 red/green 方式守护，证据见
  [RUNTIME-TRANSPORT-VALIDATION.md](RUNTIME-TRANSPORT-VALIDATION.md)。

- 用户态 amem/LSFS v0 已有 context CAS/COW、checkpoint 和 artifact commit；2026-09-18
  补齐 artifact commit → durable mailbox → headless runner 链路，binding/cursor 持久化，
  mailbox 与 cursor 原子提交，支持停用/重启/恢复扫描。全量 race、vet、Linux arm64 编译、
  engine/core smoke 及两项 mutation 验证通过，见 [RUNTIME-LSFS-WAKE-VALIDATION.md](RUNTIME-LSFS-WAKE-VALIDATION.md)。
  这一项属于用户态已实现切片，不能替代以下内核验收项。
- durable mailbox 新增未确认工作的单 Application 与 supervisor 总量准入上限；timer/LSFS
  满载时保留源状态，ack 后继续投递，支持重启续投。claim 仍占额度，retire 释放执行额度。
  这不限制已确认历史、context commit 或审计的保留量；磁盘配额仍未实现。

- 2026-09-19 P3 首切片：supervisor 污点网关——payload 声明污点位、claim 时累计进
  Application 持久台账、`application.export` 按 manifest `export_mask` 拒绝未掩蔽
  污点（`-32006`）或降级为已审计的人工确认；门控决策全部进 hash chain。见
  [RUNTIME-TAINT-VALIDATION.md](RUNTIME-TAINT-VALIDATION.md)。unotify 慢路径、
  两级撤销与 witness 联签仍属后续。

## 当前未实现

- 内核 amem/LSFS 存储、sched_ext Agent 调度、ainf 内核设备化、Web capability、HostFS bridge
  和消息 gateway 尚未实现。`0009` 的 wake target 仍是 source 私有，dormant aproc 创建与
  `ON_QUIESCENT` 语义不完整；内核 registry 不落盘，跨 VM 持久性由 supervisor snapshot/
  restore 承接；独立受监督的 fsd 进程尚未存在（当前由 supervisor 扫描器兼任生产者）。`runtime/cmd/aok-init` 与
  `kernel/initramfs/build-aok.sh` 已提供 supervisor PID1/initfs 基础闭环；QEMU kernel probe
  仍是独立验证程序，不能替代完整 guest 服务编排。
- `kernel/linux` 保持干净的上游基线；AOK 代码位于外层 patch，构建时应用到独立源码目录。
- 本机 QEMU 构建没有 virtio-vsock device model，当前只完成内核配置检查，未完成 guest↔host
  vsock 心跳；该项转移到 Apple Container 或支持 vsock 的 Linux/QEMU runner。P2 的 guest
  全栈启动已验收；节流→freeze 渐进降级与前缀命中率/亲和已有实现与证据，cache 亲和
  spawn（spawn 期放置决策）与多 slot 联合测量尚未实现。
- 本机已安装 QEMU 11.1.1，可运行 guest 测试；macOS 系统 GNU Make 为 3.81，内核编译和
  `make kernel-config-probe` 仍使用 Linux builder。
