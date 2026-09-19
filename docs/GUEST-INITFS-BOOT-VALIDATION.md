# Guest 全栈 PID1 启动验证

2026-09-19，P2 首切片：自制 initfs 上的完整用户态栈在 AOK 内核中启动并保持存活。
这是 `PLAN-AOK-DEEP.md` P2 验收项“AOK 以 PID 1 起于自制 initfs”的第一个证据，
并顺带在 guest 内整体演练了 P1 遗留的 `AOK_ROOT_FD` 传递路径。

## 启动链

```text
QEMU virt arm64 (kernel/.build/qemu-arm64-p10 Image)
└─ /init = aok-init（Go，PID1）
   · claimInitKernelRoot：认领 root capability（每 boot 一次）
   · RunInit 监督子进程；ExtraFiles 把 root fd 传为 fd 3，env AOK_ROOT_FD=3
   └─ /sbin/aok-supervisor --state /var/lib/aok --manifest /etc/aok/manifest.yaml
      · OpenKernelEventBridge 经 AOK_ROOT_FD 激活内核 application registry
      · echo provider（manifest engine=echo）+ control listener
      · 串口输出 AOK_KERNEL_BRIDGE=on、AOK_SUPERVISOR_READY=<socket>
```

## 过程修复

- `kernel/initramfs/build-aok.sh` 以默认权限创建 `/var/lib/aok`，而 supervisor
  要求状态目录 0700；initfs 此前从未真实启动故未暴露。已改为显式 chmod 0700。
- `0002` 的 `aok_root_cap_claim` 用 `task_pid_nr(current) != 1` 判定初始进程，
  对多线程 PID1（Go/Rust runtime 的 main goroutine 可能在非组长线程上执行
  syscall）会错误返回 `EPERM`。改为 `task_tgid_nr`。全 series 重新应用与构建
  验证通过。

## 验收脚本

`make aok-initfs-boot-test`（`runtime/scripts/initfs-boot.sh`）：交叉编译
aok-init/aok-supervisor（CGO_ENABLED=0，linux/arm64 静态），`make aok-initramfs`
打包，QEMU 启动后在串口日志上验收：

- `AOK_SUPERVISOR_READY=` 出现（全栈就绪）；
- `AOK_KERNEL_BRIDGE=on`（PID1 认领 + fd 传递 + 子进程激活成功）；
- 稳定窗口内 READY 标记恰好出现一次（无崩溃循环）；
- 串口无 `Kernel panic`/`Attempted to kill init`/`BUG`/`WARNING`/`Oops`。

guest 尚无关机路径（vsock 控制面属后续切片），由 harness 在验收后停止 QEMU。

## 尚未完成

- 监督重启（supervisor 崩溃后 aok-init 重启再就绪）只有 pid1 单测覆盖，尚未在
  guest 内演练；headless turn 的 guest 内证据由 `aok-event-probe-test` 提供
  （同 supervisor 栈，直接以 PID1 运行）。
- engine 仅 echo；llama/metal/anthropic 后端与 vsock 控制面、命中率与节流→freeze
  渐进降级属于 P2 后续切片。
