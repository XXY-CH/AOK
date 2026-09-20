# AOK 当前边界

这份记录把准备阶段容易混淆的名称、路径和实现状态固定下来。它与设计决议一起使用，
不替代 `docs/PLAN-AOK-DEEP.md` 的阶段计划。

2026-09-20 对照结论：P1-P5 均为**部分实现**，不能据以下切片通过情况认定整阶段完成。
原始验收标准不变；逐阶段差距见 [P1-P5-ALIGNMENT.md](P1-P5-ALIGNMENT.md)。

## 已冻结

- **AOK** 是 Agent-native kernel、SubOS 产品和 VM 环境的统一名称，Swift 可执行文件统一为 `aok`。
- `kernel/linux` 是上游 `linux-6.18.y` 的 detached shallow checkout，提交历史不与
  AOK 合并。macOS 使用大小写敏感卷承载它，根目录只保留 patch/config/kselftest。
- Agent 的产品 ABI 是 AOK fd/handle ABI。CLI、GUI、ACP、MCP、A2A 和 Linux/POSIX 用户态
  都是外部或用户态适配器；它们不能成为 Application 存活的条件。
- HostFS 的标准 guest 路径由挂载 capability 决定，示例使用 `/mnt/project/in` 和
  `/mnt/project/out`。路径字符串本身不授予权限。

## 当前已实现

- 设计文档、P0 `1.0-draft` 语义文档集、Linux 基线拉取脚本、QEMU arm64 配置探针、patch/config/kselftest 准备目录；稳定 syscall 编号和两个独立实现的互操作验收尚未完成。
- `aok up/down/ls/exec` 用于开发 VM 生命周期和探针，`exec` 尚未迁移为 handle 语义。
  Swift TUI 已通过配置的 `aokctl` 与 control socket 调用 AOK control API。
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
  snapshot 交接 supervisor 持久化，恢复时投递 mailbox。appregistry 47 项 + 全部六套
  enabled（object 44、task 64、resource 56、event-source 38、event-wake 37、
  core 46）与五套 disabled ENOSYS 回归通过，含一项游标重置 mutation 验证，见
  [KERNEL-APP-REGISTRY-VALIDATION.md](KERNEL-APP-REGISTRY-VALIDATION.md)。
  runtime 侧 `kernelbridge` 绑定 0010 ABI，supervisor 以 `kernel:<app>:<origin_boot_id>:<event_id>`
  幂等键先持久后 ack 地 drain 内核队列，满载丢弃 handle 重放，关停 snapshot/启动
  恢复。跨 VM 的旧 snapshot 按原始 boot 身份直接持久投递到 mailbox；新队列独立 drain，
  避免 Restore 竞态及新 VM 复用事件编号串入旧记录。
  生产路径已通电：PID1 认领 root 经 `AOK_ROOT_FD` 传给 supervisor，LSFS
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
  版本化 backend fallback 策略（router 按 context 的允许顺序尝试、直连 provider 调用前检查、
  越权 turn 以 `route_denied` 失败且不触达 backend），每个 turn 追加不可变 route
  record（policy 版本/backend/compat/fallbacks/cache_hit_kind/token 三项/理由码，
  supervisor 全局上限 512 条、新的逐出旧的），`application.set_route_policy` 与 `route.list` 上控制面。
- 2026-09-19 P2 切片：supervisor 推理经内核 ainf 设备闭环——`KernelInferProvider`
  把每个 turn 放入 0006 会话（预留/实报/对账/EDQUOT 全内核强制；Cancel/失败完成按
  全额预留计费并保持对账），event probe 在真实内核上验证短 turn 结算、取消后对账
  不发散与超预算 fail-closed，见
  [AOK-CORE-VALIDATION.md](AOK-CORE-VALIDATION.md) 追加节。生产接线见下方
  2026-09-20 条目。
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
  [RUNTIME-TAINT-VALIDATION.md](RUNTIME-TAINT-VALIDATION.md)。内核会话污点经
  `LastTaint()` 回流台账，外发门控同时看到 payload 与内核两类标记。unotify 慢路径、
  acapd/Cedar/Biscuit 和外部 witness 服务仍属后续；两级撤销与本地 witness 验证已见下列切片。

- 2026-09-19 P3 切片：撤销两级生效——内核级为既有 0006 revoke（probe 验证）；
  runtime 级新增 capability token 撤销注册表（digest 持久化、caveat 前缀链式波及
  全部后代、只接受可验证 token、幂等、审计），`capability.check` 查注册表、
  控制面 `capability.revoke`。见
  [RUNTIME-REVOCATION-VALIDATION.md](RUNTIME-REVOCATION-VALIDATION.md)。

- 2026-09-20 P5 用户态切片：`scripts/mvp-research.sh` 一条命令入口（自动拉起或复用
  supervisor、planner 输出传入 researcher、driver 收集结果提交 aggregator）；默认持久保留
  report.txt、完整 receipt.json 和自建 supervisor 的 state，支持 `-o` 指定目录。
  `make runtime-mvp-mixed-smoke` 是历史命名，实际只使用一个真实
  llama.cpp 后端验证——共享 brief 前缀使后续 researcher 命中率 0.95（首个 0.00
  冷启动），前缀亲和在真实 KV cache 上生效，输出归档 smoke-logs。
- 2026-09-20 P4 复审修复：网关来源映射从进程内全局改为 supervisor 私有 + 重启时从
  持久信封重建（回复跨重启可用、多实例 msg-N 无碰撞）；web 证据链记录真实最终
  URL（重定向后来源不再误标）；hostfs 挂载路径创建即规范化（尾斜杠/双斜杠挂载
  根不可达问题消除）。后续审阅发现同 channel 多 conversation 重启串路、Reply 漏污点检查、
  WebExecute 在应用校验前触网，已补修复与回归；旧的“全部安全目标正确”结论撤回。
- 2026-09-20 P5 首证据：MVP 多应用调研队列（echo 后端）——`make runtime-mvp-smoke`
  以真实 supervisor 控制面驱动 planner → 3 researchers（VTC 排序 + 前缀亲和）→
  aggregator 报告 checkpoint 落盘，审计与命中率/token 台账可复算；runner 彼时串行执行，
  echo 只验证数据流，不验证报告质量或并行公平性。
  输出归档 `kernel/.build/smoke-logs/runtime-mvp.log`。见
  [RUNTIME-MVP-VALIDATION.md](RUNTIME-MVP-VALIDATION.md)。三后端混合编排、
  工具调用、port 回传、完整 LSFS、/proc 观测与 ash 入口属后续；COW 与并行见下方
  2026-09-20 条目。
- 2026-09-19 P4 独立审查修复：网关回复按**来源会话**路由（多会话应用不再随机
  误投/跨 principal 泄漏）、入站信封展平 text 字段（runner 真正读到正文）并**强制
  external taint**（网关入口不再绕过污点台账）、重绑保留游标（无重放窗口）；web
  重定向逐跳校验前缀（scope 外 fail-closed）、前缀匹配改 host 边界（同串前缀的
  兄弟域/userinfo 技巧被拒）、`MaxResponseBytes` 真实生效、taint/audit/persist
  合一临界区；hostfs 路径归一化（`..`/`//` 穿越被拒）、import 折 external taint、
  transfer 淘汰统一策略。回归测试覆盖：headless Run 展平路由、重定向/边界/双向
  大小、穿越/五模式矩阵/重启。
- 2026-09-19 P4 切片：hostfs bridge 与 artifact transfer——持久版本化 mount
  授权（ro/rw/append-only/dropbox 双向、guest 路径前缀匹配、TTL、unmount 收紧），
  路径字符串本身不授权；artifact export 先过污点门控（未掩蔽污点拒绝）、import/
  export 均幂等并审计；六个 supervisor 方法（控制面接线属后续），跨重启保留。
- 2026-09-19 P4 切片：web capability 阶梯的 fetch/document 两层——capability
  按 URL 前缀/method/大小/TTL 收窄，越层请求被拒（`ErrWebLayerMismatch`），
  transport 由 supervisor 持有，响应携带 external-content taint 并折入应用台账
  （与污点门控闭环），每次调用审计；session/browser 层待 JS runtime。
- 2026-09-19 P4 首切片：消息网关——持久 channel/conversation 绑定外部身份到
  application，入站以 `gw:<channel>:<id>:<seq>` 幂等键进 durable mailbox 并按
  wake policy 驱动（on_event headless / manual 离线累积），出站 outbox 以
  `reply:<message_id>` 幂等、claim/ack 重连不重复投递并携带 receipt；八个控制面
  方法。见 [RUNTIME-GATEWAY-VALIDATION.md](RUNTIME-GATEWAY-VALIDATION.md)。
  真实外部 transport 与 MCP/A2A/ACP 驱动属后续。
- 2026-09-19 P3 独立审查修复：确认审批与升级 payload 绑定（防偷换）、pending
  确认队列上限 64+终局清扫（防无界状态）、撤销按 Subject+严格前缀收紧（防兄弟链
  误伤）、`message.claim` 控制面路径补污点折叠（防 RPC 绕过门控）、witness
  `Count==0` 防崩、prepared 恢复不再误折他应用的内核污点；witness 的安全条件
  （外部自留记录）已显式写入验证文档，落盘自洽重造链的拒绝有专测。
- 2026-09-19 P3 末片：witness 联签——审计检查点（链头/条数/时间）经独立 ed25519
  witness 密钥签名后持久锚定，`witness.head/cosign/verify` 上控制面；伪造链头、
  错误密钥与本地自洽重造链全部被拒（`-32008`），跨重启保留。见
  [RUNTIME-WITNESS-VALIDATION.md](RUNTIME-WITNESS-VALIDATION.md)。外部 witness
  服务本体属后续。
- 2026-09-19 P3 切片：人工确认慢路径——未掩蔽污点导出可 `escalate` 为 pending
  确认（request_id/TTL 5 分钟），`confirmation.list/settle` 审批，approved 请求
  单次放行（consumed），全部状态迁移入审计并跨重启保留；控制面
  `application.export` 以 `-32007` 返回 pending。
- 2026-09-20 修复：撤销注册表到达 1024 条时明确拒绝新增，不再逐出有效撤销；
  router 遵循应用指定顺序；内核事件按 boot 分域，回滚恢复所有可选 map 与来源索引。
- 2026-09-20 P2 收口：生产 supervisor engine 路径接入 kernel ainf——`-kernel-infer`
  开关把所选 provider（含 router）包入 `KernelInferProvider`，root 仅接受 PID1 经
  `AOK_ROOT_FD` 移交的描述符，不支持平台显式报错而非静默绕过。新增
  `make aok-initfs-turn-test`（`runtime/scripts/initfs-turn-boot.sh`）：内核命令行
  `aok_kernel_infer=1 aok_boot_probe=turn` 由 guest aok-init 解析（initramfs 无根文件
  系统，命令行是唯一配置通道；仅此两键），supervisor 生产入口以 echo 引擎经真实
  0006 设备完成一个完整 turn，串口证据
  `AOK_KERNEL_INFER=on provider=kernel-infer/echo`、
  `AOK_BOOT_TURN=pass provider=kernel-infer/echo input_tokens=19 checkpoint=<hash>`、
  READY 恰一次、串口干净（`kernel/.build/aok-initramfs/serial-initfs-turn.log`）。
  kernel-infer capability 单一：并发 turn 下第二个会话立即返回 busy 并由 runner
  重排队（不计失败、不烧 turn 时限）。同 fd 多后端切换与 vsock 心跳仍未做；
  0006 `AOK_CORE_DATA_MAX=512` 使该路径提示词上限 512 字节，真实长度需 ABI 分段
  （属后续）。
- 2026-09-20 P1/P5 切片：COW fan-out 接线——控制面 `application.fork` 封存父应用
  上下文 tail，子应用（可跨 owner）引用同一 CAS 页，继承污点台账并独立预算/邮箱，
  审计 `application.fork`；`context.pages` 返回封存页与 tail 哈希作共享证据。
  回归 `TestForkApplicationSharesContextPages`（fork 后页相同、子 turn 后父不变）、
  `TestForkApplicationInheritsTaint`（含退休父拒绝）。mvp_flow 的 researchers 改经
  fork 创建，receipt 记录 `cow_shared_pages`；echo 与真实 llama smoke 实测
  `[1, 1, 1]`。提示词组合仍是文本重放（planner 输出内联），内核 amem 语义的
  COW 不在本切片。审阅边界：persist 失败时子上下文行可能成为孤儿（与
  application.create 同型，恢复期无 GC）；跨 owner fork 在当前单 principal 本地
  控制面（socket 0600）下不做额外授权决策，`context.pages` 枚举已入审计——多
  principal 语义引入前须补 fork 授权门。
- 2026-09-20 P5 切片：runner 有界并行——`SetTurnConcurrency`/`-parallel-turns N`
  （1..64，默认 1 保持串行语义）并发执行已认领 turn；认领排序、VTC 公平、节流带
  与每应用单飞（一次最多一个 in-flight turn）不变，in-flight 不落盘（重启由
  load() 重置 claimed 消息承接）。回归 `TestRunnerParallelTurnsFansOut`（三应用
  barrier 同时进入）、`TestRunnerParallelSingleFlightPerApplication`（同应用
  max concurrent == 1）。mvp-research.sh 自建 supervisor 默认
  `-parallel-turns 3`。独立审阅修复：per-turn 路由与内核污点改经
  `TrackedProvider.CompleteTracked` 随调用返回（`LastRoute`/`LastTaint` 的
  latest-wins 读取在并行下会张冠李戴）；kernel-infer 单 slot 占用时返回
  `ErrTurnBusy`，runner 重排队且不计失败。回归
  `TestRunnerParallelTurnsBounded`（并发上界）、
  `TestRunnerParallelTrackedAttribution`（并行下逐 turn 路由归属）、
  `TestRunnerBusyTurnRequeuesWithoutFailure`。
- 2026-09-20 P2 切片：多 slot 联合测量——`make runtime-mvp-multislot-smoke`
  （`runtime/scripts/mvp-multislot-smoke.py`）：llama-server `-np 4 -cb` +
  supervisor `-parallel-turns 4`，轮询 `/slots` 断言后端同时处理 ≥2 slot。
  实测 `max_busy_slots=4`、4 turn 批次 164ms 对单条 79ms（串行下界约 316ms，
  仅报告不作断言）、无 deny；多 slot 下 KV 命中率为逐 slot 行为（0.93/0.93/0/0），
  不作断言。supervisor 侧并发语义由 Go 回归覆盖。见
  [RUNTIME-MULTISLOT-VALIDATION.md](RUNTIME-MULTISLOT-VALIDATION.md)。
- 2026-09-20 P3 切片：manifest→launcher 编译器——`sandbox.Compile` 把 manifest 的
  fs/net/tools 编译为 Landlock+seccomp launcher 计划：路径规则仅接受绝对路径加
  可选 `/**`、禁空白，网络默认拒绝（无 `--net` 时 launcher 保持 socket 过滤），
  tools 声明不编译为 execute 规则，launcher 与引擎命令必须绝对路径。编译器为
  aok-supervisor `-engine-command` 路径的生产入口；guest 内端到端强制与 unotify、
  acapd 仍属后续。

## 尚未完成

- 内核 amem/LSFS 存储、sched_ext Agent 调度仍未实现；ainf 已有内核 0006 + probe
  闭环，生产 supervisor 已接入 KernelInferProvider（2026-09-20，见上），同 fd 多后端
  切换与多 slot 联合验收未做。Web fetch/document、HostFS 授权与传输数据模型、
  消息 gateway 队列已有用户态实现，但完整控制面、协议适配器和真实外部 transport 未齐备。
  `0009` 的 wake target 仍是 source 私有，dormant aproc 创建与
  `ON_QUIESCENT` 语义不完整；内核 registry 不落盘，跨 VM 持久性由 supervisor snapshot
  和 mailbox 恢复承接；独立受监督的 fsd 进程尚未存在（当前由 supervisor 扫描器兼任生产者）。`runtime/cmd/aok-init` 与
  `kernel/initramfs/build-aok.sh` 已提供 supervisor PID1/initfs 基础闭环；QEMU kernel probe
  仍是独立验证程序，不能替代完整 guest 服务编排。
- `kernel/linux` 保持干净的上游基线；AOK 代码位于外层 patch，构建时应用到独立源码目录。
- 本机 QEMU 构建没有 virtio-vsock device model，当前只完成内核配置检查，未完成 guest↔host
  vsock 心跳；该项转移到 Apple Container 或支持 vsock 的 Linux/QEMU runner。P2 的 guest
  全栈启动已验收；节流→freeze 渐进降级与前缀命中率/亲和已有实现与证据，多 slot 联合
  测量已有 smoke 证据（2026-09-20）；cache 亲和 spawn（spawn 期放置决策）尚未实现。
- 本机已安装 QEMU 11.1.1，可运行 guest 测试；macOS 系统 GNU Make 为 3.81，内核编译和
  `make kernel-config-probe` 仍使用 Linux builder。
