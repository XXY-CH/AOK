# Kernel Configurations

Store generated, reviewable configuration fragments here. The first probe targets
QEMU arm64 and records support for `sched_ext`, Landlock, virtiofs, AF_VSOCK, and
the arm64 virtual machine path. Apple Containerization is validated after the QEMU
baseline is reproducible.

`qemu-arm64-aok.fragment` is the minimum P1 feature set. `probe-stock.sh` merges it
into an out-of-tree `defconfig`, checks the resulting symbols, and refuses to run on
macOS because Linux Kconfig requires GNU Make >= 4.0. The generated output belongs in
`kernel/.build/` and is not committed.
