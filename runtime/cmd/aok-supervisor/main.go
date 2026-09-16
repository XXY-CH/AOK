package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	aok "aok/runtime"
)

type stringListFlag []string

func (s *stringListFlag) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringListFlag) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("state", "", "private durable state directory")
	manifest := flag.String("manifest", "", "capability manifest")
	endpoint := flag.String("llama-endpoint", "", "supervisor-owned llama.cpp service")
	engineCommand := flag.String("engine-command", "", "optional supervised engine executable")
	engineArgs := stringListFlag{}
	flag.Var(&engineArgs, "engine-arg", "argument passed to --engine-command (repeatable)")
	maxTokens := flag.Int("max-tokens", 128, "maximum model output tokens per turn")
	flag.Parse()
	if *root == "" || *manifest == "" {
		return fmt.Errorf("state and manifest are required")
	}
	policy, err := aok.LoadCapabilitySet(*manifest)
	if err != nil {
		return err
	}
	var provider aok.Provider
	if *engineCommand != "" {
		p, processErr := aok.NewProcessProvider(aok.ProcessProviderConfig{Command: *engineCommand, Args: engineArgs})
		if processErr != nil {
			return processErr
		}
		defer p.Close()
		provider = p
	} else {
		switch policy.Engine {
		case "echo":
			provider = aok.EchoProvider{}
		case "llama":
			provider, err = aok.NewLlamaProvider(aok.LlamaConfig{Endpoint: *endpoint, MaxTokens: *maxTokens, CachePrompt: true})
		default:
			return fmt.Errorf("unsupported engine %q", policy.Engine)
		}
	}
	if err != nil {
		return err
	}
	s, err := aok.NewSupervisor(*root, policy)
	if err != nil {
		return err
	}
	defer s.Close()
	path := filepath.Join(*root, "control.sock")
	// State lock is held. A stale socket from a crashed instance can be removed,
	// but ordinary files are never removed on behalf of a socket configuration.
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("control path is not a socket")
		}
		if err = os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	l, err := aok.NewControlListener(path)
	if err != nil {
		return err
	}
	defer l.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- (&aok.ControlServer{Supervisor: s, Listener: l}).Serve(ctx) }()
	go func() { done <- s.Run(ctx, provider) }()
	fmt.Printf("AOK_SUPERVISOR_READY=%s\n", path)
	err = <-done
	cancel()
	other := <-done
	if err != nil {
		return err
	}
	return other
}
