# GLM 实现任务：aproc task/pidfd 生命周期

## 目标

在 Linux 6.18.51 AOK fork 上实现第三枚内核切片：一个 `aproc` 管理一个或多个 Linux
`task_struct`，创建时返回首个 task 的 pidfd，子 task 默认继承 aproc，支持显式 pidfd attach、
freeze、resume、abort 和 reap。完成后生成独立 patch，不直接修改 AOK 外层仓库的文档或
`kernel/patches/series`。

## 输入与基线

- 上游提交：`f6388029ea9e2c9e807d73827658738ea131faee`（Linux 6.18.51）。
- 前置补丁必须按顺序应用：
  `0001-aok-object-handle-core.patch`、`0002-aok-root-capability-bootstrap.patch`。
- 参考语义：[docs/abi/object-model.md](abi/object-model.md)、[docs/abi/syscalls.md](abi/syscalls.md)、
  [docs/abi/p0-freeze.md](abi/p0-freeze.md)。

## 必须实现

1. 扩展 `struct aok_aproc`，保存受引用保护的 task 集合、状态、终态和父 capability 约束；
   AID/koid 仍由 kernel 生成且不复用。
2. 将 `aok_aproc_create()` 改为原子创建 aproc、首个 Linux task 和 pidfd。首个 task 的
   初始入口/用户栈可以采用最小测试 ABI，但必须明确返回结构和错误语义。
3. 新增 `aok_aproc_spawn(aproc_fd, attr)`，在同一 aproc 中创建额外 task；普通
   `fork/clone` 子 task 默认继承 aproc，不能通过普通 syscall 清除归属。
4. 新增 `aok_aproc_attach(aproc_fd, pidfd)`；必须验证目标 pidfd、`ATTACH` right、PID
   namespace 和 job policy，不能用 PID 数字授权。
5. 新增 `aok_aproc_freeze()` / `aok_aproc_resume()` / `aok_aproc_abort()` /
   `aok_aproc_reap()`。freeze 使用 Linux freezer 停止所有绑定 task；resume 返回 runtime
   可恢复级别 `none|messages|messages+kv`，但 kernel 不保存 KV。
6. task 退出、exec 或 pidfd 关闭不得自动销毁 aproc；只有显式 reap 或策略终止才进入终态。
   终态和 task 变化必须进入有序 event queue，至少包含 aid、task koid、event_seq 和状态。
7. 所有新增输入结构遵循 `size`/追加字段/零尾部规则；权限只能收窄；fd 发布必须在
   copyout 成功后完成，失败路径不能泄漏 task、pidfd、fd slot 或引用。

## 测试验收

新增独立 kselftest，至少覆盖：

- create 返回 aproc fd + pidfd，inspect 能看到 task 数量和状态；
- spawn 两个 task，fork/clone 子 task 继承 aproc；
- attach 无 `ATTACH` 或错误 pidfd 被拒绝，跨 namespace 拒绝；
- freeze 后 task 不再运行，resume 恢复；没有 runtime checkpoint 时返回 `none`；
- task 退出后 aproc 仍可 inspect，reap 后进入终态；重复 reap 幂等或返回文档指定错误；
- pidfd close 不销毁 aproc，AID 不复用；
- copyout fault、fd exhaustion、并发 freeze/reap 不泄漏或 UAF；
- 配置关闭时新增 syscall 返回 `ENOSYS`。

## 禁止越界

本任务不实现 CPU/内存/token 资源域、sched_ext、event source、amem、ainf、LSFS、acap、
supervisor、GUI/CLI、MCP/ACP 或 Apple Containerization。不要把 POSIX signal、UID 0、
普通 cgroup 或 PID 字符串当作 Agent capability。不要重写前两枚补丁的历史。

## 交付物

- `0003-aok-aproc-task-pidfd.patch`，可在干净基线加前两枚补丁后独立 `git apply`。
- kselftest 源码及一条可运行命令。
- 一份测试输出：编译 warning/error、TAP 结果、QEMU 串口日志和补丁 SHA-256。
- 明确列出仍未实现的 ABI 项，不得宣称 Agent 已完成持久化。
