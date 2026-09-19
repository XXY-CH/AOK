//go:build !linux || !arm64

package runtime

import "os"

func claimInitKernelRoot() *os.File { return nil }
