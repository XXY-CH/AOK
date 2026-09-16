# QEMU arm64 stock probe

这是 P1 的第一条可复现启动路径。它验证上游内核和设备模型；AOK 对象 syscall 使用独立的
对象测试配置和 runner，见下文。

## 配置

在 Linux builder 上执行：

```sh
make linux-mount
make linux-fetch
kernel/configs/probe-stock.sh
```

macOS 可用 `AOK_ALLOW_DARWIN=1 MAKE=gmake kernel/configs/probe-stock.sh` 完成 config-only
准备；实际 kernel build 和 boot 仍必须在 Linux 上执行。

脚本使用 `arch/arm64/configs/defconfig`，再合并
[`qemu-arm64-aok.fragment`](qemu-arm64-aok.fragment)，所有生成文件写入
`kernel/.build/qemu-arm64-stock`。它会检查 `sched_ext`、Landlock、virtiofs、virtio-vsock、
KVM、initrd 和串口配置；不会修改 `kernel/linux` 工作树。

## 启动探针

配置通过后，构建内核和模块：

```sh
make -C kernel/linux O=kernel/.build/qemu-arm64-stock ARCH=arm64 Image modules dtbs
```

启动所需的最小探针 initramfs 可用 `make qemu-initramfs` 构建；它只执行 `/init` 和设备探针，
不代表 AOK runtime 已实现。启动验收要求串口输出保存为
`kernel/.build/qemu-arm64-stock/serial.log`。启动验收至少
包括：

- `/proc/cmdline`、`/proc/config.gz` 或保存的 `.config` 与提交号；
- `cat /sys/kernel/sched_ext/state`（启用 sched_ext 后由 BPF scheduler 接管时可见）；
- securityfs 中的 LSM 列表；Landlock syscall ABI 需由专用 probe 程序调用
  `landlock_create_ruleset(2)` 验证，不能仅凭目录存在判断；
- `test -e /dev/vsock` 或 `modprobe vmw_vsock_virtio_transport` 后建立 AF_VSOCK socket；
- virtiofs 设备枚举，挂载测试在提供 `virtiofsd` 后执行；
- freeze、vsock 心跳和 hostfs 测试结果分别记录，不能用“内核启动”代替。

可用 `make qemu-boot` 重复启动 10 秒并打印 `AOK_PROBE_*` 行；`AOK_QEMU_SECONDS`、
`AOK_QEMU_MEMORY` 和 `AOK_QEMU_SMP` 可覆盖默认值。当前 Homebrew QEMU 11.1.1 的
`-device help` 没有 virtio-vsock device model，故本机日志中的 `AOK_PROBE_VSOCK=absent`
只表示没有注入设备；内核配置 `CONFIG_VSOCKETS=y` 和 `CONFIG_VIRTIO_VSOCKETS=y` 已通过，
需在提供 vsock 设备的 QEMU/Linux runner 或 Apple Container 中完成连接测试。

本机 macOS 已安装 QEMU 11.1.1，可执行启动验证；系统 `make` 仍为 3.81，内核编译使用
Linux builder。CI 应固定 Linux 发行版、QEMU 版本、GNU Make 版本和内核 commit，并将
配置、串口和设备探针作为构建产物保存。

第一枚对象补丁的测试使用独立最小配置 `qemu-arm64-object.fragment`，入口和证据见
[`../kselftest/README.md`](../kselftest/README.md)。该配置只验证对象基础，不替代本页的
完整设备配置和后续资源域验证。
