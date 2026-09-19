//go:build linux && arm64

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	aok "aok/runtime"
	"aok/runtime/kernelbridge"
)

// runABI drives the 0010 ABI directly: register, post with a cursor, read,
// snapshot, ack, restore and verify the id space continues.
func runABI(registry *kernelbridge.Registry) error {
	app, err := registry.OpenApplication(9001)
	if err != nil {
		return fmt.Errorf("open application: %w", err)
	}
	defer app.Close()
	source, err := registry.OpenEventSource(kernelbridge.SourceLSFS, 0, 0)
	if err != nil {
		return fmt.Errorf("open lsfs source: %w", err)
	}
	defer source.Close()
	if err := source.Attach(app, 9001); err != nil {
		return fmt.Errorf("attach: %w", err)
	}
	if err := source.PostLSFS(0x2a); err != nil {
		return fmt.Errorf("post: %w", err)
	}
	event, err := app.Read()
	if err != nil || event == nil {
		return fmt.Errorf("read: %v %v", event, err)
	}
	if event.Data != 0x2a || event.Kind != kernelbridge.KindLSFS {
		return fmt.Errorf("unexpected record: %+v", event)
	}
	snapshot, err := app.Snapshot()
	if err != nil || len(snapshot) != 1 {
		return fmt.Errorf("snapshot: %v %+v", err, snapshot)
	}
	if err := app.Ack(event.EventID); err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	if left, _ := app.Snapshot(); len(left) != 0 {
		return fmt.Errorf("ack left %d events", len(left))
	}
	if err := app.Restore(snapshot); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	// A fresh handle replays the restored queue from its own cursor.
	fresh, err := registry.OpenApplication(9001)
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	defer fresh.Close()
	replayed, err := fresh.Read()
	if err != nil || replayed == nil || replayed.EventID != event.EventID {
		return fmt.Errorf("restore replay: %+v %v", replayed, err)
	}
	if err := source.PostLSFS(0x2b); err != nil {
		return fmt.Errorf("post after restore: %w", err)
	}
	next, err := fresh.Read()
	if err != nil || next == nil || next.EventID <= event.EventID {
		return fmt.Errorf("id space did not continue: %+v %v", next, err)
	}
	fmt.Printf("AOK_EVENT_PROBE_ABI=pass id=%d seq=%d data=0x%x\n",
		event.EventID, event.EventSeq, event.Data)
	return nil
}

// runSupervisor drives the production wiring: LSFS bindings route artifact
// commits through the kernel durable queue, the runner drains them into the
// mailbox and finishes echo turns, and an orderly restart replays the rest.
func runSupervisor(root *kernelbridge.Root, stateDir string) error {
	bridge, err := aok.KernelEventBridgeFromRoot(root)
	if err != nil {
		return fmt.Errorf("bridge: %w", err)
	}
	s, err := aok.NewSupervisor(stateDir, aok.CapabilitySet{Engine: "echo"})
	if err != nil {
		return fmt.Errorf("supervisor: %w", err)
	}
	s.SetKernelBridge(bridge)
	app, err := s.CreateApplication("probe", "probe-owner", "on_event")
	if err != nil {
		return fmt.Errorf("application: %w", err)
	}
	if err := s.AddLSFSBinding("probe", app.ApplicationID, "b1", "", 0); err != nil {
		return fmt.Errorf("binding: %w", err)
	}
	if _, err := s.ImportArtifact("probe", app.ApplicationID, "k1",
		json.RawMessage(`{"text":"hello kernel"}`)); err != nil {
		return fmt.Errorf("import: %w", err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx, aok.EchoProvider{}) }()
	if err := waitForKernelTurns(s, app.ApplicationID, 1, 15*time.Second); err != nil {
		stop()
		<-done
		s.Close()
		return err
	}
	queues, err := bridge.Open(app.KernelID)
	if err != nil {
		stop()
		<-done
		s.Close()
		return fmt.Errorf("queue handle: %w", err)
	}
	left, _ := queues.Snapshot()
	queues.Close()
	if len(left) != 0 {
		stop()
		<-done
		s.Close()
		return fmt.Errorf("kernel queue not drained: %+v", left)
	}
	stop()
	<-done
	// An artifact committed after the runner stops stays unacked in the
	// kernel; the restart must deliver it exactly once.
	if _, err := s.ImportArtifact("probe", app.ApplicationID, "k2",
		json.RawMessage(`{"text":"after restart"}`)); err != nil {
		s.Close()
		return fmt.Errorf("import k2: %w", err)
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	s2, err := aok.NewSupervisor(stateDir, aok.CapabilitySet{Engine: "echo"})
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	defer s2.Close()
	s2.SetKernelBridge(bridge)
	runCtx2, stop2 := context.WithCancel(context.Background())
	done2 := make(chan error, 1)
	go func() { done2 <- s2.Run(runCtx2, aok.EchoProvider{}) }()
	defer func() { stop2(); <-done2 }()
	if err := waitForKernelTurns(s2, app.ApplicationID, 2, 15*time.Second); err != nil {
		return err
	}
	mailbox, err := s2.ListMailbox(app.ApplicationID)
	if err != nil {
		return err
	}
	kernelCount := 0
	for _, m := range mailbox {
		if strings.HasPrefix(m.IdempotencyKey, "kernel:") {
			kernelCount++
		}
	}
	if kernelCount != 2 {
		return fmt.Errorf("expected 2 kernel events total, got %d", kernelCount)
	}
	fmt.Printf("AOK_EVENT_PROBE_SUPERVISOR=pass app=%s events=%d\n",
		app.ApplicationID, kernelCount)
	return nil
}

// waitForKernelTurns polls until `want` kernel-routed events exist and every
// one of them has been turned and acknowledged by the runner.
func waitForKernelTurns(s *aok.Supervisor, appID string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		mailbox, err := s.ListMailbox(appID)
		if err != nil {
			return err
		}
		seen, acked := 0, 0
		for _, m := range mailbox {
			if !strings.HasPrefix(m.IdempotencyKey, "kernel:") {
				continue
			}
			seen++
			if m.Status == "acked" {
				acked++
			}
		}
		if seen == want && acked == want {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%d kernel turns did not complete in %s", want, timeout)
}

func run() error {
	if os.Getpid() != 1 {
		return errors.New("event probe must run as guest PID1")
	}
	for _, mount := range []struct{ path, fs string }{{"/proc", "proc"}, {"/sys", "sysfs"}, {"/dev", "devtmpfs"}} {
		if err := os.MkdirAll(mount.path, 0755); err != nil {
			return err
		}
		if err := syscall.Mount(mount.fs, mount.path, mount.fs, 0, ""); err != nil {
			return err
		}
	}
	root, err := kernelbridge.Bootstrap()
	if err != nil {
		return err
	}
	defer root.Close()
	registry := kernelbridge.RegistryFromRoot(root)
	if err := runABI(registry); err != nil {
		return fmt.Errorf("abi phase: %w", err)
	}
	if err := runSupervisor(root, filepath.Join("/var", "lib", "aok-event-probe")); err != nil {
		return fmt.Errorf("supervisor phase: %w", err)
	}
	fmt.Println("AOK_EVENT_PROBE=pass")
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Printf("AOK_EVENT_PROBE=fail reason=%v\n", err)
		time.Sleep(200 * time.Millisecond)
		os.Exit(1)
	}
	// Let QEMU's poweroff marker flush before exiting PID1.
	time.Sleep(200 * time.Millisecond)
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}
