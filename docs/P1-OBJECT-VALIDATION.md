# P1 对象切片验证

日期：2026-09-15。状态：前两枚实验对象/bootstrap 补丁已验证；完整 P1 未完成。

## 结果

`0001-aok-object-handle-core.patch` 与 `0002-aok-root-capability-bootstrap.patch` 已通过从固定上游提交导出、独立应用、完整编译和
QEMU guest syscall 测试，加入 `kernel/patches/series`。上游 `kernel/linux` 工作树及
index 均 clean；未修改上游提交历史。外层仓库的既有未提交文档和文件保持未提交。

| 检查 | 结果 |
|---|---|
| clean 基线 `git apply --check` | 通过 |
| 独立源码编译启用/禁用两种 Image | 通过，构建日志未发现 warning/error |
| 实验对象/bootstrap kselftest | 43 pass，0 fail，0 skip |
| 配置禁用 kselftest | 4 pass，均返回 `ENOSYS` |
| 并发句柄操作 | 4 线程共 4000 次 duplicate/inspect/close；最后几份引用并发关闭另测 |
| 测试 runner 负向检查 | 错误模式和 0 秒超时均返回非零退出码 |
| `checkpatch.pl --strict` | 0 errors；忽略新增文件的 `FILE_PATH_CHANGES` 提示后 0 warnings/0 checks |
| Shell 语法 | build/run 脚本通过 `sh -n` |
| 独立静态审查 | 未发现实验合同内的正确性缺陷 |

## 环境与证据

- Linux：6.18.51，`f6388029ea9e2c9e807d73827658738ea131faee`。
- Builder：Debian 12 arm64，GCC 12.2.0，GNU Make 4.3，GNU ld 2.40。
- Builder image：`debian:bookworm-slim`，arm64 manifest digest
  `sha256:6bd27d44e6c32a66bbd72d7cb2b76a8ae3497ec2e5274a81abd1b37f6013fa1f`。
- QEMU：11.1.1，`virt` / `cortex-a72`，2 vCPU，512 MiB，macOS 上软件模拟。
- 配置：`kernel/configs/qemu-arm64-object.fragment`，默认关闭实验功能，测试配置显式开启。
- Patch SHA-256：`0001` 为 `e4bee31c1976f689559ea8e328b9b165e81dfbf7bf54625ec934e89abce2d158`；
  `0002` 为 `ff32a5119649e8bf78c99db320a6a9bd30bef7eb1b3c33c1981d0df31e90144f`。
- 重建日志：[object-repro-build.log](../kernel/.build/object-repro-build.log)。
- 启用串口：[p2-final-serial.log](../kernel/.build/qemu-arm64-object/p2-final-serial.log)。
- 禁用串口：[p2-disabled.log](../kernel/.build/qemu-arm64-object/p2-disabled.log)。
- 构建配置和产物 hashes：[build-manifest.txt](../kernel/.build/qemu-arm64-object/build-manifest.txt)。

生成证据位于被 gitignore 的 `.build`，不会把二进制加入源码仓库。重新构建时可能因构建
时间、主机名或包版本改变得到不同 Image hash；本记录证明流程可复现，不承诺字节可重复构建。
运行入口见 [kselftest README](../kernel/kselftest/README.md)。

## 实现边界

这两枚补丁只分配无 task 的 `CREATED` 对象，并初始化一次性 root capability；不宣称首个
task、pidfd、资源域、freezer、事件源或持久 Agent 已实现。权限包含 `MANAGE_CHILD`，但
Linux dup/fork/传递仍遵循 POSIX 规则，不提供线性能力或撤销。
`DUPLICATE`；Linux dup/fork/传递仍遵循 POSIX 规则，不提供线性能力或撤销。

未做内存分配失败的 fault injection、KASAN/KCSAN/lockdep 验证或 AID 饱和的运行测试。
本次内核是对象专用最小配置，不代替 sched_ext、memcg、vsock、virtiofs 或 Apple microVM
集成验收。后续依赖顺序：task/pidfd 生命周期 ->
资源域与事件源；每个切片继续以独立 patch 和 guest kselftest 验收。
