package runtime

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type processTestProvider string

func (p processTestProvider) Name() string { return string(p) }
func (p processTestProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	switch string(p) {
	case "crash":
		os.Exit(23)
	case "slow":
		<-ctx.Done()
		return "", Usage{}, ctx.Err()
	}
	return EchoProvider{}.Complete(ctx, prompt)
}

func TestProcessProviderChild(t *testing.T) {
	args := os.Args
	if len(args) < 3 || args[len(args)-2] != "--aok-child" {
		return
	}
	if args[len(args)-1] == "hang" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
	file := os.NewFile(3, "listener")
	listener, err := net.FileListener(file)
	file.Close()
	if err != nil {
		os.Exit(24)
	}
	err = Serve(context.Background(), listener, func() Provider { return processTestProvider(args[len(args)-1]) })
	if err != nil {
		os.Exit(25)
	}
	os.Exit(0)
}

func testProcessProvider(t *testing.T, mode string, maxStarts int) *ProcessProvider {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := NewProcessProvider(ProcessProviderConfig{Command: executable, Args: []string{"-test.run=^TestProcessProviderChild$", "--aok-child", mode}, StartupTimeout: time.Second, RequestTimeout: 3 * time.Second, ShutdownTimeout: 10 * time.Millisecond, MaxStarts: maxStarts})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func TestProcessProviderEchoAndReap(t *testing.T) {
	p := testProcessProvider(t, "echo", 3)
	for _, prompt := range []string{"first", "second"} {
		text, usage, err := p.Complete(context.Background(), prompt)
		if err != nil || text != prompt || usage.OutputTokens != uint64(len(prompt)) {
			t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
		}
	}
	child := p.child
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(child.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket directory remains: %v", err)
	}
	if err := syscall.Kill(child.cmd.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("child remains: %v", err)
	}
	if _, _, err := p.Complete(context.Background(), "closed"); err == nil {
		t.Fatal("closed provider accepted prompt")
	}
}

func TestProcessProviderCancellation(t *testing.T) {
	p := testProcessProvider(t, "slow", 3)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := p.Complete(ctx, "blocked")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error: %v", err)
	}
	if time.Since(start) > 2*time.Second || p.child != nil {
		t.Fatal("cancel did not reap child promptly")
	}
}

func TestProcessProviderCrashIntensity(t *testing.T) {
	p := testProcessProvider(t, "crash", 2)
	for i := 0; i < 2; i++ {
		if _, _, err := p.Complete(context.Background(), "crash"); err == nil {
			t.Fatal("crash accepted")
		}
		if p.child != nil {
			t.Fatal("crashed child not reaped")
		}
	}
	if _, _, err := p.Complete(context.Background(), "crash"); !errors.Is(err, ErrRestartIntensity) {
		t.Fatalf("restart limit: %v", err)
	}
}

func TestProcessProviderStartupTimeout(t *testing.T) {
	p := testProcessProvider(t, "hang", 3)
	p.config.StartupTimeout = 50 * time.Millisecond
	start := time.Now()
	if _, _, err := p.Complete(context.Background(), "hello"); err == nil {
		t.Fatal("hung engine accepted")
	}
	if time.Since(start) > time.Second || p.child != nil {
		t.Fatal("hung startup was not reaped")
	}
}

func TestProcessProviderConcurrentCalls(t *testing.T) {
	p := testProcessProvider(t, "echo", 3)
	var workers sync.WaitGroup
	for i := 0; i < 6; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			text, _, err := p.Complete(context.Background(), "parallel")
			if err != nil || text != "parallel" {
				t.Errorf("text=%q err=%v", text, err)
			}
		}()
	}
	workers.Wait()
	if len(p.starts) != 1 {
		t.Fatalf("started %d processes", len(p.starts))
	}
}

func TestProcessProviderRealEchoBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds real engine binary")
	}
	binary := filepath.Join(t.TempDir(), "echo")
	build := exec.Command("go", "build", "-o", binary, "./cmd/aok-engine-echo")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	p, err := NewProcessProvider(ProcessProviderConfig{Command: binary})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	text, usage, err := p.Complete(context.Background(), "real subprocess")
	if err != nil || text != "real subprocess" || usage.OutputTokens != 15 {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestProcessProviderRejectsInvalidConfig(t *testing.T) {
	_, err := NewProcessProvider(ProcessProviderConfig{Command: "relative"})
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("invalid config: %v", err)
	}
}
