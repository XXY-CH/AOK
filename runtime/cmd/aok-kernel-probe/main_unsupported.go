//go:build !linux || !arm64

package main

import (
	"aok/runtime/kernelbridge"
	"fmt"
	"os"
)

func main() { fmt.Fprintln(os.Stderr, kernelbridge.ErrUnsupported); os.Exit(1) }
