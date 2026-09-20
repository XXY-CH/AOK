//go:build !linux || !arm64

package runtime

import "os"

func claimInitKernelRoot() *os.File { return nil }

func prepareInitKernel() error { return nil }

// PrepareInitKernel is a no-op off the linux/arm64 guest path.
func PrepareInitKernel() error { return nil }
