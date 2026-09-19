# AOK Application Model

## 定义

Application 是面向 Agent 的可组合能力单元。它把一个 Agent 当前需要的功能、状态、
工具引用和资源预算组织成一个可寻址对象；它不是给人类界面包装的进程，也不等同于
Linux process、engine 或 aproc。

Application 的长期身份存入 LSFS。运行时可以有多个短生命周期的执行化身：

```text
Application identity
  -> versioned contract and durable state
  -> aproc incarnation
  -> Linux task / engine session
```

其中：

- `application_id` 是 LSFS 命名空间内稳定的 UUID，删除后不复用；
- `aproc` 是当前 boot 内的资源和生命周期对象，拥有不可伪造的 AID；
- `engine` 是实现 Application 行为的可替换后端；
- Linux task 是执行细节，退出不等于 Application 终止。

## Agent-first 约束

每个 Application 必须由 Agent 需求驱动，至少声明以下契约：

```yaml
application_id: uuid
owner_agent: agent_uuid
contract_version: 1
inputs: [message, artifact, event]
outputs: [message, artifact, event]
capabilities: [memory.read, tool.search]
resources:
  cpu_usec: 1000000
  memory_bytes: 536870912
  token_reserve: 32768
lifecycle:
  wake: on_message
  checkpoint: turn_boundary
  retention: until_owner_forgets
```

能力必须通过 capability handle 授予；路径、Application ID 或用户界面选择都不是权限
证明。资源使用进入对应 Agent/job/aproc 账户，Application 不得绕过 AOK 记账。

Application 的默认交互面是 channel、memory、tool 和 event。CLI、GUI 和 ACP 只能作为
可选的观察、批准或调试 client，不能成为 Application 继续工作的前提。

## 主动唤醒

Application 的 `wake_policy` 绑定一组 `event_source`，事件源可为 `timer`、`port` 或
LSFS commit/mailbox。事件到达后，runtime 先把事件写入 durable mailbox，再根据 Application
状态执行：

```text
frozen  -> resume current aproc
dormant -> create new aproc/generation
failed  -> restart worker, restore checkpoint, replay pending events
```

事件绑定的是持久 `application_id`，不是临时 AID。事件采用有序序号、幂等 `event_id` 和
显式 ack；未 ack 的事件在资源不足、客户端断开、worker crash 或 VM reboot 后仍可 replay。
事件投递仍受 capability、supervisor policy 和 CPU/内存/token 资源域约束。

当前用户态 v0 只实现 timer 和 LSFS artifact commit 的持久 mailbox 投递。LSFS binding
与 scan cursor 随 supervisor 恢复，runner 的 append/checkpoint 不产生唤醒事件。
manual/frozen Application 会积累 pending，必须按现有管理流程处理或 resume。
`0009` 已单独实现内核 timer/port 恢复显式授权的 frozen aproc；`0010` 加入 boot 内
durable 的 Application registry：内核队列按 `application_id` 跨 source fd 与进程
replay，supervisor 以幂等键 drain 进 durable mailbox 并在关停/启动时 snapshot/
restore。dormant aproc 创建、按持久 owner 的内核侧隔离与完整恢复链路仍待集成。
用户态 mailbox 满载时，LSFS 保留受阻事件前的
cursor、timer 保留原 due，确认消息释放容量后续投；retire 将未确认消息标为 expired。
具体控制契约见 [control-api.md](control-api.md)。

## 生命周期

```text
requested -> growing -> serving -> adapting -> quiescent
     |          |          |           |
     +----------+----------+-----------+
                    |
                 retiring -> tombstoned
```

- `requested`：Agent 提出能力缺口，supervisor 校验策略和预算；
- `growing`：创建或升级 contract、memory schema、tool bindings 和执行化身；
- `serving`：处理消息和 turn；
- `adapting`：Agent 根据反馈增删能力或替换 engine，使用新 `contract_version`；
- `quiescent`：没有工作时可冻结、休眠或回收执行化身，持久状态仍保留；
- `retiring`：Agent 或策略明确不再需要该 Application，先停止新请求并完成 checkpoint；
- `tombstoned`：写入不可复用的墓碑记录，保留审计和被引用版本，之后才能按 LSFS GC 规则
  回收无引用对象。

`freeze`、worker crash、VM 重启和 engine 替换只能改变执行化身状态。只有显式 retire 或
不可恢复的策略终止才结束 Application 身份。恢复时 supervisor 读取 `application_id`、
contract、checkpoint 和 durable mailbox，创建新的 aproc/AID，并 replay 未确认消息。

## 生长、迭代与消失

Application 的变化必须可观测、可回滚：

1. **生长**：新增 capability、tool、memory index 或 worker，产生新版本并通过父 capability
   收窄授权；旧版本可以继续服务已有 turn。
2. **迭代**：以 `contract_version` 和 migration record 发布兼容或不兼容变更。不可兼容变更
   先建立新版本，再迁移未完成消息和可迁移状态。
3. **消失**：先 quiesce，再拒绝新请求、checkpoint、撤销 capability、写 tombstone，最后
   由可达性 GC 回收没有审计或能力引用的对象。

每个外部副作用都必须带 `turn_id` 和 `request_id` 幂等键。恢复或 replay 时，Application
可以确认已完成的副作用而不重复执行。

## 观测与控制

Application 通过 `application.inspect`、`application.freeze`、`application.resume`、
`application.update` 和 `application.retire` 暴露给 AOK control API；这些方法最终映射到
supervisor 持有的 capability，而不是让 CLI/GUI 直接调用 kernel root capability。

`events.subscribe` 至少报告 `requested`、`serving`、`adapting`、`quiescent`、`retiring`、
`tombstoned`、`worker_restarted`、`checkpoint_committed` 和 `replay_completed`。事件必须
带单调序号和 `application_id`，与 aproc 的 AID/事件流分开命名。
