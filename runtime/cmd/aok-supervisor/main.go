package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	aok "aok/runtime"
	"aok/runtime/sandbox"
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
	metalEndpoint := flag.String("metal-endpoint", "", "supervisor-owned local Metal llama.cpp service")
	anthropicEndpoint := flag.String("anthropic-endpoint", "", "Anthropic Messages API endpoint")
	anthropicModel := flag.String("anthropic-model", "", "Anthropic model name")
	routerEngines := stringListFlag{}
	flag.Var(&routerEngines, "router-engine", "router provider in fallback order (llama or anthropic; repeatable)")
	engineCommand := flag.String("engine-command", "", "optional supervised engine executable")
	sandboxLauncher := flag.String("sandbox-launcher", "", "optional absolute AOK Landlock/seccomp launcher")
	engineArgs := stringListFlag{}
	flag.Var(&engineArgs, "engine-arg", "argument passed to --engine-command (repeatable)")
	maxTokens := flag.Int("max-tokens", 128, "maximum model output tokens per turn")
	kernelInfer := flag.Bool("kernel-infer", false, "route every engine turn through the kernel inference device (requires AOK_ROOT_FD)")
	kernelInferTokens := flag.Uint64("kernel-infer-tokens", 1<<20, "kernel capability token budget when -kernel-infer is set")
	parallelTurns := flag.Int("parallel-turns", 1, "maximum turns executing concurrently (1 keeps strictly serial execution)")
	cgroupRoot := flag.String("cgroup-root", "", "optional writable cgroup v2 root for kernel-enforced limits")
	cgroupName := flag.String("cgroup-name", "aok-supervisor", "cgroup leaf name")
	flag.Parse()
	if *root == "" || *manifest == "" {
		return fmt.Errorf("state and manifest are required")
	}
	if *parallelTurns < 1 || *parallelTurns > 64 {
		return fmt.Errorf("parallel-turns must be between 1 and 64")
	}
	policy, err := aok.LoadCapabilitySet(*manifest)
	if err != nil {
		return err
	}
	var provider aok.Provider
	var resources *aok.CgroupController
	if *cgroupRoot != "" {
		resources, err = aok.NewCgroupController(*cgroupRoot, *cgroupName)
		if err != nil {
			return err
		}
		defer resources.Close()
		if err = resources.Configure(aok.ResourceLimits{CPUs: uint64(policy.CPUs), MemoryBytes: uint64(policy.MemMiB) << 20}); err != nil {
			return err
		}
		if err = resources.AttachPID(os.Getpid()); err != nil {
			return err
		}
	}
	if *engineCommand != "" {
		// The manifest compiles into the launcher plan up front; the spawn
		// path re-composes the argv with its delegated listener fd.
		plan, compileErr := sandbox.Compile(sandbox.Manifest{
			FSRead: policy.FSRead, FSWrite: policy.FSWrite, Net: policy.Net, Tools: policy.Tools,
		}, *sandboxLauncher, append([]string{*engineCommand}, engineArgs...), []int{3})
		if compileErr != nil {
			return compileErr
		}
		p, processErr := aok.NewProcessProvider(aok.ProcessProviderConfig{Command: *engineCommand, Args: engineArgs, ResourceController: resources, SandboxLauncher: plan.Launcher, SandboxPolicy: plan.Policy})
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
		case "metal":
			provider, err = aok.NewMetalProvider(aok.MetalConfig{Endpoint: *metalEndpoint, MaxTokens: *maxTokens})
		case "anthropic":
			provider, err = aok.NewAnthropicProvider(aok.AnthropicConfig{Endpoint: *anthropicEndpoint, Model: *anthropicModel, MaxTokens: *maxTokens})
		case "router":
			provider, err = newRouter(*endpoint, *metalEndpoint, *anthropicEndpoint, *anthropicModel, *maxTokens, routerEngines)
		default:
			return fmt.Errorf("unsupported engine %q", policy.Engine)
		}
	}
	if err != nil {
		return err
	}
	if *kernelInfer {
		wrapped, wrapErr := aok.WrapKernelInferProvider(provider, *kernelInferTokens, uint64(*maxTokens))
		if wrapErr != nil {
			return fmt.Errorf("kernel infer: %w", wrapErr)
		}
		fmt.Printf("AOK_KERNEL_INFER=on provider=%s budget=%d\n", wrapped.Name(), *kernelInferTokens)
		if closer, ok := wrapped.(io.Closer); ok {
			defer closer.Close()
		}
		provider = wrapped
	}
	s, err := aok.NewSupervisor(*root, policy)
	if err != nil {
		return err
	}
	defer s.Close()
	if err = s.SetTurnConcurrency(*parallelTurns); err != nil {
		return err
	}
	if bridge, err := aok.OpenKernelEventBridge(); err == nil {
		s.SetKernelBridge(bridge)
		fmt.Printf("AOK_KERNEL_BRIDGE=on\n")
	} else {
		fmt.Printf("AOK_KERNEL_BRIDGE=off reason=%v\n", err)
	}
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
	if resources != nil {
		go func() {
			_ = resources.Monitor(ctx, aok.ResourceLimits{CPUs: uint64(policy.CPUs), MemoryBytes: uint64(policy.MemMiB) << 20}, 100*time.Millisecond, nil)
		}()
	}
	done := make(chan error, 2)
	go func() { done <- (&aok.ControlServer{Supervisor: s, Listener: l}).Serve(ctx) }()
	go func() { done <- s.Run(ctx, provider) }()
	fmt.Printf("AOK_SUPERVISOR_READY=%s\n", path)
	// Guest boot acceptance (AOK_BOOT_PROBE=turn): drive one full turn
	// through the production control plane and engine path, then report on
	// the serial console. A failed probe exits the supervisor so PID1's
	// restart policy surfaces the fault instead of a silent degradation.
	if os.Getenv("AOK_BOOT_PROBE") == "turn" {
		probeDone := make(chan error, 1)
		go func() { probeDone <- runBootProbe(ctx, path, provider.Name()) }()
		select {
		case err = <-probeDone:
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err != nil {
			fmt.Printf("AOK_BOOT_TURN=fail reason=%v\n", err)
			cancel()
			<-done
			<-done
			return err
		}
	}
	err = <-done
	cancel()
	other := <-done
	if err != nil {
		return err
	}
	return other
}

// runBootProbe executes one application turn over the local control socket
// and checks the engine actually answered through the expected provider.
func runBootProbe(ctx context.Context, socket, expectProviderPrefix string) error {
	client, err := aok.DialControl(socket)
	if err != nil {
		return err
	}
	defer client.Close()
	var app aok.Application
	if err = client.Call(ctx, "application.create", map[string]any{"owner_agent": "boot-probe", "wake_policy": "on_event"}, &app); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	prompt := "AOK boot probe turn"
	var sent aok.MailboxMessage
	if err = client.Call(ctx, "message.send", map[string]any{
		"application_id": app.ApplicationID, "idempotency_key": "boot-probe",
		"payload": map[string]string{"text": prompt},
	}, &sent); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	var result aok.TurnResult
	deadline := time.Now().Add(60 * time.Second)
	for {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = client.Call(callCtx, "message.result", map[string]string{
			"application_id": app.ApplicationID, "message_id": sent.MessageID}, &result)
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("turn did not finish: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if result.Status != "completed" || result.Text != prompt || result.Usage.InputTokens != uint64(len(prompt)) {
		return fmt.Errorf("unexpected turn result: %+v", result)
	}
	if !strings.HasPrefix(result.Provider, expectProviderPrefix) {
		return fmt.Errorf("turn provider %q does not run through %q", result.Provider, expectProviderPrefix)
	}
	if result.Checkpoint == "" {
		return fmt.Errorf("turn committed no checkpoint")
	}
	var audit []aok.AuditRecord
	if err = client.Call(ctx, "audit.list", map[string]string{}, &audit); err != nil {
		return err
	}
	for _, record := range audit {
		if record.Decision == "deny" {
			return fmt.Errorf("audit deny during probe: %+v", record)
		}
	}
	fmt.Printf("AOK_BOOT_TURN=pass provider=%s input_tokens=%d checkpoint=%s\n",
		result.Provider, result.Usage.InputTokens, result.Checkpoint)
	return nil
}

func newRouter(llamaEndpoint, metalEndpoint, anthropicEndpoint, anthropicModel string, maxTokens int, order []string) (aok.Provider, error) {
	if len(order) == 0 {
		order = []string{"llama", "anthropic"}
	}
	providers := make([]aok.Provider, 0, len(order))
	for _, name := range order {
		switch name {
		case "llama":
			if llamaEndpoint == "" {
				return nil, fmt.Errorf("router llama provider requires --llama-endpoint")
			}
			p, err := aok.NewLlamaProvider(aok.LlamaConfig{Endpoint: llamaEndpoint, MaxTokens: maxTokens, CachePrompt: true})
			if err != nil {
				return nil, err
			}
			providers = append(providers, p)
		case "anthropic":
			p, err := aok.NewAnthropicProvider(aok.AnthropicConfig{Endpoint: anthropicEndpoint, Model: anthropicModel, MaxTokens: maxTokens})
			if err != nil {
				return nil, err
			}
			providers = append(providers, p)
		case "metal":
			p, err := aok.NewMetalProvider(aok.MetalConfig{Endpoint: metalEndpoint, MaxTokens: maxTokens})
			if err != nil {
				return nil, err
			}
			providers = append(providers, p)
		default:
			return nil, fmt.Errorf("unsupported router provider %q", name)
		}
	}
	return aok.NewRouterProvider(providers...)
}
