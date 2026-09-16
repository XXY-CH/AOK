# AOK Kernel Workspace

`kernel/linux` is the detached upstream `linux-6.18.y` baseline. On macOS it is
mounted from `.aok-linux-hfs.sparseimage`, a case-sensitive local volume required
because the upstream tree contains paths that differ only by case. AOK changes stay
outside that history until a patch is ready to apply and test.

The preparation layout is:

- `patches/series`: ordered, reviewable AOK patch list.
- `configs/`: reproducible QEMU arm64 and Apple guest configuration notes.
- `kselftest/`: AOK-specific tests and their acceptance probes.
- `linux/`: upstream Linux source tree, never edited by preparation commands.

Use `make linux-mount` after a fresh checkout (the target creates the local sparse
image on macOS) and `make linux-fetch` to update the shallow baseline. The sparse
image and nested checkout are local preparation state, not files to commit into the
AOK repository.

P1 starts with a stock kernel boot in QEMU arm64. Only after that probe is recorded
do the first patches add AOK objects, resource domains, and event sources.
