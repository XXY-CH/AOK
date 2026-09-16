package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRunInitStopsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	err := RunInit(ctx, InitConfig{Supervisor: filepath.Join("/", "bin", "sh"), SupervisorArgs: []string{"-c", "trap 'exit 0' TERM; sleep 5"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunInit error=%v", err)
	}
}

func TestRunInitRestartIntensity(t *testing.T) {
	err := RunInit(context.Background(), InitConfig{Supervisor: filepath.Join("/", "bin", "sh"), SupervisorArgs: []string{"-c", "exit 1"}, Restart: true, MaxRestarts: 2, RestartWindow: time.Second})
	if !errors.Is(err, ErrRestartIntensity) {
		t.Fatalf("RunInit error=%v", err)
	}
}
