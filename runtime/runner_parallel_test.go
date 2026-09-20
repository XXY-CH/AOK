package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// barrierProvider releases only when the requested number of turns run at
// the same time; it proves real concurrency without wall-clock flakiness.
type barrierProvider struct {
	barrier chan struct{}
	entered chan struct{}
}

func (p *barrierProvider) Name() string { return "barrier" }

func (p *barrierProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	select {
	case p.entered <- struct{}{}:
	case <-ctx.Done():
		return "", Usage{}, ctx.Err()
	}
	select {
	case <-p.barrier:
		return prompt, Usage{InputTokens: uint64(len(prompt)), OutputTokens: uint64(len(prompt))}, nil
	case <-ctx.Done():
		return "", Usage{}, ctx.Err()
	}
}

// TestRunnerParallelTurnsFansOut asserts that with turn concurrency three,
// three single-message applications execute their turns simultaneously.
func TestRunnerParallelTurnsFansOut(t *testing.T) {
	s := supervisorForTest(t, t.TempDir())
	if err := s.SetTurnConcurrency(3); err != nil {
		t.Fatal(err)
	}
	provider := &barrierProvider{barrier: make(chan struct{}), entered: make(chan struct{}, 3)}
	type pending struct {
		applicationID string
		messageID     string
	}
	var messages []pending
	for i := 0; i < 3; i++ {
		a, err := s.CreateApplication("admin", "research", "on_event")
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.Enqueue("admin", a.ApplicationID, "r", json.RawMessage(`{"text":"focus"}`))
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, pending{applicationID: a.ApplicationID, messageID: m.MessageID})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, provider) }()
	for i := 0; i < 3; i++ {
		select {
		case <-provider.entered:
		case <-time.After(5 * time.Second):
			cancel()
			t.Fatal("turns did not run concurrently")
		}
	}
	close(provider.barrier)
	deadline := time.Now().Add(5 * time.Second)
	for _, p := range messages {
		for {
			if _, err := s.Result(p.applicationID, p.messageID); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("turns did not finish after barrier release")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// overlapProvider tracks the maximum number of simultaneous calls.
type overlapProvider struct {
	current, max atomic.Int64
}

func (p *overlapProvider) Name() string { return "counting" }

func (p *overlapProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	current := p.current.Add(1)
	for {
		max := p.max.Load()
		if current <= max || p.max.CompareAndSwap(max, current) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	p.current.Add(-1)
	return prompt, Usage{InputTokens: uint64(len(prompt)), OutputTokens: uint64(len(prompt))}, nil
}

// TestRunnerParallelSingleFlightPerApplication asserts that parallel fan-out
// never runs two turns of the same application at once.
func TestRunnerParallelSingleFlightPerApplication(t *testing.T) {
	s := supervisorForTest(t, t.TempDir())
	if err := s.SetTurnConcurrency(4); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateApplication("admin", "research", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err = s.Enqueue("admin", a.ApplicationID, fmt.Sprintf("m%d", i), json.RawMessage(fmt.Sprintf(`{"text":"turn %d"}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	provider := &overlapProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, provider) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var acknowledged int
		s.mu.Lock()
		for _, m := range s.state.Mailbox[a.ApplicationID] {
			if m.Status == "acked" {
				acknowledged++
			}
		}
		s.mu.Unlock()
		if acknowledged == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turns did not finish: acked=%d", acknowledged)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if provider.max.Load() != 1 {
		t.Fatalf("application turns overlapped: max concurrent=%d", provider.max.Load())
	}
}
