package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	aok "aok/runtime"
)

type stringList []string

func (s *stringList) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := aok.RunInit(ctx, aok.InitConfig{Supervisor: *supervisor, SupervisorArgs: args, Restart: *restart, MaxRestarts: *maxRestarts}); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
