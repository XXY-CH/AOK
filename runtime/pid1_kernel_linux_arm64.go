//go:build linux && arm64

package runtime

import (
	"fmt"
	"os"

	"aok/runtime/kernelbridge"
	"golang.org/x/sys/unix"
)

// PrepareInitKernel mounts /proc when running as guest PID1 so kernel
// configuration (boot identity, command line) is readable. It is idempotent
// and a no-op off the init seat; RunInit calls it again before spawning.
func PrepareInitKernel() error { return prepareInitKernel() }

func prepareInitKernel() error {
	if os.Getpid() != 1 {
		return nil
	}
	if _, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return nil
	}
	if err := os.MkdirAll("/proc", 0555); err != nil {
		return err
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount proc for kernel boot identity: %w", err)
	}
	return nil
}

// claimInitKernelRoot claims the AOK root capability exactly once per boot.
// Only PID1 may claim it; the descriptor is handed to the supervisor child
// via AOK_ROOT_FD. A nil return means this kernel has no AOK ABI and the
// child stays on the user-space path.
func claimInitKernelRoot() *os.File {
	root, err := kernelbridge.Bootstrap()
	if err != nil {
		return nil
	}
	return root.File()
}
