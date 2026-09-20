package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	aok "aok/runtime"
)

type stringList []string

func (s *stringList) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// bootOverrides reads the guest kernel command line, the configuration
// channel of a root-filesystem-less initramfs. Only two keys exist, both
// for boot acceptance: aok_kernel_infer=1 adds -kernel-infer to the
// supervisor arguments; aok_boot_probe=turn sets AOK_BOOT_PROBE=turn so the
// supervisor drives one turn and reports on the serial console. No other
// command-line token influences the child.
func bootOverrides() (kernelInfer bool, bootProbe bool) {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false, false
	}
	for _, token := range strings.Fields(string(data)) {
		switch token {
		case "aok_kernel_infer=1":
			kernelInfer = true
		case "aok_boot_probe=turn":
			bootProbe = true
		}
	}
	return kernelInfer, bootProbe
}

func main() {
	supervisor := flag.String("supervisor", "/sbin/aok-supervisor", "absolute supervisor executable")
	args := stringList{}
	flag.Var(&args, "supervisor-arg", "argument passed to supervisor (repeatable)")
	restart := flag.Bool("restart", false, "restart the supervisor after an unexpected exit")
	maxRestarts := flag.Int("max-restarts", 3, "maximum starts in the restart window")
	flag.Parse()
	if !filepath.IsAbs(*supervisor) {
		fmt.Fprintln(os.Stderr, "supervisor must be an absolute path")
		os.Exit(2)
	}
	if len(args) == 0 {
		args = stringList{"--state", "/var/lib/aok", "--manifest", "/etc/aok/manifest.yaml"}
	}
	// Mount /proc first: the command line is the configuration channel of
	// this root-filesystem-less initramfs, and only PID1 may mount it.
	if err := aok.PrepareInitKernel(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var env []string
	kernelInfer, bootProbe := bootOverrides()
	if kernelInfer {
		args = append(args, "-kernel-infer")
	}
	if bootProbe {
		env = append(env, "AOK_BOOT_PROBE=turn")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := aok.RunInit(ctx, aok.InitConfig{Supervisor: *supervisor, SupervisorArgs: args, SupervisorEnv: env, Restart: *restart, MaxRestarts: *maxRestarts}); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
