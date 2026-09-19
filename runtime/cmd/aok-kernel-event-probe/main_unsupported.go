//go:build !linux || !arm64

package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	fmt.Printf("AOK_EVENT_PROBE=fail reason=%v\n",
		errors.New("kernel event probe requires Linux arm64 with CONFIG_AOK_EXPERIMENTAL"))
	os.Exit(1)
}
