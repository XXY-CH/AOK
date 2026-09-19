//go:build !linux || !arm64

package runtime

import "aok/runtime/kernelbridge"

// OpenKernelEventBridge reports the kernel ABI as unavailable; the
// supervisor runs with the user-space durable mailbox only.
func OpenKernelEventBridge() (KernelEventBridge, error) {
	return nil, kernelbridge.ErrUnsupported
}
