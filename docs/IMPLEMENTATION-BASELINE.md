# AOK Implementation Baseline

状态：准备阶段决议。本文把第一次代码提交前的实现边界固定下来。

## 内核基线

- 上游来源：`https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git`
- 分支：`linux-6.18.y`
- 工作树：`kernel/linux`。macOS 上必须挂载到大小写敏感卷；本地准备脚本使用
  `.aok-linux-hfs.sparseimage`，避免 Linux 内核中大小写不同的合法路径互相覆盖。
- AOK 改动：外层维护 `kernel/patches/series`、`kernel/configs/` 和独立
  `kernel/kselftest/`；不改写上游提交历史。
- 验证顺序：QEMU arm64，然后 Apple Containerization microVM。调研中出现的 `6.14.9+`
  是当前 Containerization guest 的兼容性验证下限，不是 AOK fork 基线。

根目录 `Makefile` 提供 `linux-mount`、`linux-fetch` 和 `linux-status`；上游工作树及其
大小写敏感卷不进入 AOK Git 历史。

选择 `linux-6.18.y` 是为了使用 stable/LTS 维护线，并在同一上游线上验证 `sched_ext`、
Landlock、virtiofs、AF_VSOCK 和 arm64 KVM/QEMU 支持。实际配置能力以 probe 结果为准。

## 旧运行时的处置

迁移期 Go `agentd` 原型不再是产品运行路径，清理范围包括：Go `ProcTable`、旧 engine
registry、agentd PID1、aok-ctl、`/run/aok/syscall.sock` JSON-RPC server/client、依赖它们
的旧 OCI rootfs、交叉编译二进制和生成报告，以及 host CLI 中直接 `spawn/ps` 旧 agentd 的
路径。对应语义仍保留在架构和 ABI 文档中，仅作为迁移参考。

未来如果需要接入旧用户态，必须放在命名明确的 `adapters/legacy/`，不能重新成为 AOK PID1
或默认 control API。当前 host CLI 只保留为后续 control API/GUI adapter 的位置，不承诺旧
命令仍可运行。

## 第一次实现的启动链

1. 编译未修改的 `linux-6.18.y` arm64 内核并在 QEMU 启动，记录 config、串口和 vsock 探针结果。
2. 建立最小 AOK patch series：对象/handle、aproc、resource domain、event source；每个 patch
   具有独立 kselftest 和可回滚边界。
3. 以 AOK PID1/initfs 和 runtime handshake 替代 agentd。Apple Containerization 接入前，
   先在 QEMU 完成 freeze/resume、virtiofs、vsock 和 capability smoke test。
4. host CLI/GUI 只连接 control API，不直接调用 Linux socket、PID 或旧 JSON-RPC。

## 外部探针

以下是环境事实，不是架构待决：目标 config 是否启用 `sched_ext`、Landlock ABI 6、virtiofs、
AF_VSOCK；QEMU arm64 与 Apple guest 的设备参数差异；Containerization 当前版本能否加载自定义
initfs/kernel image；以及 ainf 后端的 usage 和 slot save/restore 能力。

探针失败只能触发文档中已有的降级路径，不能把旧 agentd 或 POSIX socket 重新提升为内核 ABI。
