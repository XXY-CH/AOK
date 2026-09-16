package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRunnerHeadlessTimerBudget(t *testing.T) {
	s := supervisorForTest(t, t.TempDir())
	a, err := s.CreateApplication("admin", "agent", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetTokenLimit("admin", a.ApplicationID, 4); err != nil {
		t.Fatal(err)
	}
	if err = s.AddTimer("admin", a.ApplicationID, "tick", time.Millisecond, 0, json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, EchoProvider{}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		state, err := s.InspectApplication(a.ApplicationID)
		if err != nil {
			t.Fatal(err)
		}
		if state.Checkpoint != "" {
			s.mu.Lock()
			var messageID string
			for id := range s.state.Results {
				messageID = id
			}
			s.mu.Unlock()
			r, err := s.Result(a.ApplicationID, messageID)
			if err != nil || r.Text != "hi" || r.Status != "budget_exhausted" || state.State != "frozen" || state.TokensUsed != 4 {
				t.Fatalf("state=%+v result=%+v error=%v", state, r, err)
			}
			if s.SetApplicationState("admin", a.ApplicationID, "serving") == nil {
				t.Fatal("exhausted application resumed")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("headless timer did not execute")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = VerifyAudit(s.Audit()); err != nil {
		t.Fatal(err)
	}
}
