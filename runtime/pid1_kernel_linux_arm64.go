//go:build linux && arm64

package runtime

import (
	"os"

	"aok/runtime/kernelbridge"
)

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
