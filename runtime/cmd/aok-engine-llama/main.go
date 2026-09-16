package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	engine "aok/runtime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	fd := flag.Int("listener-fd", 3, "Unix stream listener inherited from supervisor")
	endpoint := flag.String("endpoint", "http://127.0.0.1:8080", "llama.cpp server base URL")
	maxTokens := flag.Int("max-tokens", 128, "maximum generated tokens per turn")
	timeout := flag.Duration("timeout", 2*time.Minute, "backend request timeout")
	cache := flag.Bool("cache-prompt", false, "allow backend prompt KV cache reuse")
	flag.Parse()
	provider, err := engine.NewLlamaProvider(engine.LlamaConfig{Endpoint: *endpoint, MaxTokens: *maxTokens, Timeout: *timeout, CachePrompt: *cache})
	if err != nil {
		return err
	}
	if *fd < 3 {
		return fmt.Errorf("listener-fd must be an inherited descriptor >= 3")
	}
	file := os.NewFile(uintptr(*fd), "engine-listener")
	if file == nil {
		return fmt.Errorf("invalid listener descriptor")
	}
	listener, err := net.FileListener(file)
	file.Close()
	if err != nil {
		return fmt.Errorf("inherit listener: %w", err)
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return engine.Serve(ctx, listener, func() engine.Provider { return provider })
}
