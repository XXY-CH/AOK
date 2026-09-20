package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// InitConfig describes the one child owned by the AOK guest PID1.
// SupervisorArgs are passed verbatim and cannot be selected by a guest request.
// Env entries are appended to the child environment (KEY=VALUE form).
type InitConfig struct {
	Supervisor     string
	SupervisorArgs []string
	SupervisorEnv  []string
	Restart        bool
	MaxRestarts    int
	RestartWindow  time.Duration
	StopTimeout    time.Duration
}

// RunInit supervises the configured child until shutdown. It is intentionally
// small: PID1 owns process lifetime while the child owns AOK policy/state.
func RunInit(ctx context.Context, config InitConfig) error {
	if config.Supervisor == "" || !filepath.IsAbs(config.Supervisor) {
		return errors.New("init supervisor must be an absolute executable path")
	}
	if config.MaxRestarts == 0 {
		config.MaxRestarts = 3
	}
	if config.RestartWindow == 0 {
		config.RestartWindow = time.Minute
	}
	if config.StopTimeout == 0 {
		config.StopTimeout = 5 * time.Second
	}
	if config.MaxRestarts < 1 || config.RestartWindow <= 0 || config.StopTimeout <= 0 {
		return errors.New("invalid init restart limits")
	}
	if err := prepareInitKernel(); err != nil {
		return err
	}
	var starts []time.Time
	// The root capability is claimable only by PID1; keep it for the whole
	// boot and hand each supervisor incarnation the same descriptor.
	kernelRoot := claimInitKernelRoot()
	for {
		now := time.Now()
		kept := starts[:0]
		for _, started := range starts {
			if now.Sub(started) < config.RestartWindow {
				kept = append(kept, started)
			}
		}
		starts = kept
		if len(starts) >= config.MaxRestarts {
			return ErrRestartIntensity
		}
		starts = append(starts, now)
		cmd := exec.Command(config.Supervisor, config.SupervisorArgs...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if kernelRoot != nil {
			cmd.ExtraFiles = []*os.File{kernelRoot}
			cmd.Env = append(os.Environ(), "AOK_ROOT_FD=3")
		}
		if len(config.SupervisorEnv) > 0 {
			cmd.Env = append(cmd.Env, config.SupervisorEnv...)
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		wait := make(chan error, 1)
		go func() { wait <- cmd.Wait() }()
		select {
		case err := <-wait:
			if !config.Restart {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
			timer := time.NewTimer(config.StopTimeout)
			select {
			case <-wait:
				timer.Stop()
			case <-timer.C:
				_ = cmd.Process.Kill()
				<-wait
			}
			return ctx.Err()
		}
	}
}
