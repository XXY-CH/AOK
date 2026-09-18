package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func wakeApplication(t *testing.T, s *Supervisor, policy string) Application {
	t.Helper()
	a, err := s.CreateApplication("admin", "agent", policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddLSFSBinding("admin", a.ApplicationID, "artifacts", "", 0); err != nil {
		t.Fatal(err)
	}
	return a
}

func importWakeArtifact(t *testing.T, s *Supervisor, id, key string) string {
	t.Helper()
	handle, err := s.ImportArtifact("admin", id, key, json.RawMessage(`{"report":"ready"}`))
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func TestLSFSWakeRestartAndHeadlessCompletion(t *testing.T) {
	root := t.TempDir()
	s := supervisorForTest(t, root)
	a := wakeApplication(t, s, "on_event")
	handle := importWakeArtifact(t, s, a.ApplicationID, "report")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = supervisorForTest(t, root)
	if err := s.deliverLSFS(); err != nil {
		t.Fatal(err)
	}
	mailbox, err := s.ListMailbox(a.ApplicationID)
	if err != nil || len(mailbox) != 1 {
		t.Fatalf("lost offline commit: %+v %v", mailbox, err)
	}
	var event LSFSWakeEvent
	if err := json.Unmarshal(mailbox[0].Payload, &event); err != nil || event.Commit.Handle != handle || event.Commit.ContextID != a.ContextID || event.Text == "" {
		t.Fatalf("invalid wake payload: %+v %v", event, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = supervisorForTest(t, root)
	if got := importWakeArtifact(t, s, a.ApplicationID, "report"); got != handle {
		t.Fatal("import retry changed handle")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, EchoProvider{}) }()
	deadline := time.Now().Add(2 * time.Second)
	var result TurnResult
	for time.Now().Before(deadline) {
		result, err = s.Result(a.ApplicationID, mailbox[0].MessageID)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if runErr := <-done; runErr != nil {
		t.Fatal(runErr)
	}
	if err != nil || result.Text != event.Text || result.Status != "completed" || result.Checkpoint == "" {
		t.Fatalf("headless completion: %+v %v", result, err)
	}
	if err := s.deliverLSFS(); err != nil {
		t.Fatal(err)
	}
	mailbox, err = s.ListMailbox(a.ApplicationID)
	if err != nil || len(mailbox) != 1 || mailbox[0].Status != "acked" {
		t.Fatalf("duplicate delivery or writeback feedback: %+v %v", mailbox, err)
	}
	commits, err := s.contexts.Commits(a.OwnerAgent, a.ContextID, 0, 100)
	if err != nil || len(commits) != 3 {
		t.Fatalf("expected artifact, result, checkpoint: %+v %v", commits, err)
	}
	bindings, err := s.ListLSFSBindings(a.ApplicationID)
	if err != nil || len(bindings) != 1 || bindings[0].Cursor != commits[2].Cursor {
		t.Fatalf("cursor failed to pass writebacks: %+v %v", bindings, err)
	}
	if err := VerifyAudit(s.Audit()); err != nil {
		t.Fatal(err)
	}
}

func TestLSFSWakeAtomicRollback(t *testing.T) {
	s := supervisorForTest(t, t.TempDir())
	a := wakeApplication(t, s, "manual")
	importWakeArtifact(t, s, a.ApplicationID, "one")
	before, err := json.Marshal(s.state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_wake BEFORE UPDATE ON supervisor_state BEGIN SELECT RAISE(FAIL, 'injected persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.deliverLSFS(); err == nil {
		t.Fatal("delivery succeeded despite failed persistence")
	}
	after, err := json.Marshal(s.state)
	if err != nil || string(before) != string(after) {
		t.Fatal("failed transaction changed mailbox, cursor, sequence or audit")
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_wake`); err != nil {
		t.Fatal(err)
	}
	if err := s.deliverLSFS(); err != nil {
		t.Fatal(err)
	}
	first, _ := s.ListMailbox(a.ApplicationID)
	// Simulate a stale cursor: the durable event identity must deduplicate it.
	b := s.state.Bindings[a.ApplicationID+":artifacts"]
	b.Cursor = 0
	s.state.Bindings[a.ApplicationID+":artifacts"] = b
	if err := s.deliverLSFS(); err != nil {
		t.Fatal(err)
	}
	second, _ := s.ListMailbox(a.ApplicationID)
	if len(first) != 1 || !reflect.DeepEqual(first, second) {
		t.Fatalf("retry was lost or duplicated: %+v %+v", first, second)
	}
}

func TestLSFSWakeLifecycleAndOwnership(t *testing.T) {
	for _, policy := range []string{"manual", "on_event"} {
		t.Run(policy, func(t *testing.T) {
			root := t.TempDir()
			s := supervisorForTest(t, root)
			a := wakeApplication(t, s, policy)
			if policy == "on_event" {
				if err := s.SetApplicationState("admin", a.ApplicationID, "frozen"); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.SetLSFSBindingEnabled("admin", a.ApplicationID, "artifacts", false); err != nil {
				t.Fatal(err)
			}
			importWakeArtifact(t, s, a.ApplicationID, "disabled")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = supervisorForTest(t, root)
			if err := s.deliverLSFS(); err != nil {
				t.Fatal(err)
			}
			m, err := s.ListMailbox(a.ApplicationID)
			if err != nil || len(m) != 0 {
				t.Fatalf("disabled source delivered: %+v %v", m, err)
			}
			if err := s.SetLSFSBindingEnabled("admin", a.ApplicationID, "artifacts", true); err != nil {
				t.Fatal(err)
			}
			if err := s.deliverLSFS(); err != nil {
				t.Fatal(err)
			}
			m, err = s.ListMailbox(a.ApplicationID)
			if err != nil || len(m) != 1 || m[0].Status != "pending" {
				t.Fatalf("reenabled source lost event: %+v %v", m, err)
			}
			if id, _, err := s.claimTurn(); err != nil || id != "" {
				t.Fatalf("manual/frozen application ran: %s %v", id, err)
			}
			foreign, err := s.contexts.Create("other-owner", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AddLSFSBinding("admin", a.ApplicationID, "foreign", foreign, 0); !errors.Is(err, ErrContextAccess) {
				t.Fatalf("cross-owner binding allowed: %v", err)
			}
			if _, err := s.Enqueue("admin", a.ApplicationID, m[0].IdempotencyKey, m[0].Payload); err == nil {
				t.Fatal("external caller can use source key namespace")
			}
			importWakeArtifact(t, s, a.ApplicationID, "before-retire")
			if err := s.RetireApplication("admin", a.ApplicationID); err != nil {
				t.Fatal(err)
			}
			if err := s.deliverLSFS(); err != nil {
				t.Fatal(err)
			}
			bindings, err := s.ListLSFSBindings(a.ApplicationID)
			if err != nil || len(bindings) != 1 || bindings[0].Enabled {
				t.Fatalf("retired source remains enabled: %+v %v", bindings, err)
			}
			m, err = s.ListMailbox(a.ApplicationID)
			if err != nil || len(m) != 1 {
				t.Fatalf("retired source delivered: %+v %v", m, err)
			}
			if err := s.SetLSFSBindingEnabled("admin", a.ApplicationID, "artifacts", true); !errors.Is(err, ErrApplicationRetired) {
				t.Fatalf("retired source enabled: %v", err)
			}
		})
	}
}

func TestLSFSWakeBatchAndCursor(t *testing.T) {
	s := supervisorForTest(t, t.TempDir())
	a, err := s.CreateApplication("admin", "agent", "manual")
	if err != nil {
		t.Fatal(err)
	}
	importWakeArtifact(t, s, a.ApplicationID, "skip")
	commits, err := s.contexts.Commits(a.OwnerAgent, a.ContextID, 0, 1)
	if err != nil || len(commits) != 1 {
		t.Fatal(commits, err)
	}
	if err := s.AddLSFSBinding("admin", a.ApplicationID, "artifacts", a.ContextID, commits[0].Cursor); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < wakeScanBatch+1; i++ {
		importWakeArtifact(t, s, a.ApplicationID, fmt.Sprintf("batch-%d", i))
	}
	for _, want := range []int{wakeScanBatch, wakeScanBatch + 1, wakeScanBatch + 1} {
		if err := s.deliverLSFS(); err != nil {
			t.Fatal(err)
		}
		m, err := s.ListMailbox(a.ApplicationID)
		if err != nil || len(m) != want {
			t.Fatalf("batch got %d want %d: %v", len(m), want, err)
		}
		var previous int64
		for _, message := range m {
			var event LSFSWakeEvent
			if err := json.Unmarshal(message.Payload, &event); err != nil || event.Commit.Cursor <= previous {
				t.Fatal("commit ordering", err)
			}
			previous = event.Commit.Cursor
		}
	}
}
