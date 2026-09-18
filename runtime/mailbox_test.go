package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Seed through the same admission path in one transaction to avoid rewriting
// the entire snapshot for each fixture message.
func seedMailbox(t *testing.T, s *Supervisor, ids []string, count int, payload json.RawMessage) []MailboxMessage {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var first []MailboxMessage
	for _, id := range ids {
		for i := 0; i < count; i++ {
			m, err := s.enqueueLocked("test", id, fmt.Sprintf("seed-%d", i), payload)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = append(first, m)
			}
		}
	}
	if err := s.persistLocked(nil); err != nil {
		t.Fatal(err)
	}
	return first
}

func TestMailboxCapacityLimitsAndRecovery(t *testing.T) {
	for _, tc := range []struct {
		name      string
		apps      int
		count     int
		bytes     int
		aggregate bool
	}{
		{"application-count", 1, maxApplicationMailboxMessages, 2, false},
		{"application-bytes", 1, maxApplicationMailboxBytes / maxTextBytes, maxTextBytes, false},
		{"supervisor-count", maxSupervisorMailboxMessages / maxApplicationMailboxMessages, maxApplicationMailboxMessages, 2, true},
		{"supervisor-bytes", maxSupervisorMailboxBytes / maxApplicationMailboxBytes, maxApplicationMailboxBytes / maxTextBytes, maxTextBytes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			s := supervisorForTest(t, root)
			var ids []string
			for i := 0; i < tc.apps+1; i++ {
				a, err := s.CreateApplication("admin", "agent", "manual")
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, a.ApplicationID)
			}
			payload := json.RawMessage(`"` + strings.Repeat("x", tc.bytes-2) + `"`)
			first := seedMailbox(t, s, ids[:tc.apps], tc.count, payload)[0]
			target := ids[0]
			if tc.aggregate {
				target = ids[tc.apps]
			}
			assertFull := func() {
				t.Helper()
				sequence, audit := s.state.NextSequence, len(s.state.Audit)
				if _, err := s.Enqueue("admin", target, "blocked", json.RawMessage(`{}`)); !errors.Is(err, ErrMailboxFull) {
					t.Fatalf("overflow admitted: %v", err)
				}
				if s.state.NextSequence != sequence || len(s.state.Audit) != audit {
					t.Fatal("refusal mutated sequence or audit")
				}
			}
			assertFull()
			if duplicate, err := s.Enqueue("admin", ids[0], first.IdempotencyKey, payload); err != nil || duplicate.MessageID != first.MessageID {
				t.Fatal("full mailbox rejected idempotent retry", err)
			}
			if _, err := s.Enqueue("admin", ids[0], first.IdempotencyKey, json.RawMessage(`true`)); !errors.Is(err, ErrDuplicateMessage) {
				t.Fatal("capacity masked conflicting retry", err)
			}
			if _, err := s.ClaimMailbox("worker", ids[0]); err != nil {
				t.Fatal(err)
			}
			assertFull()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = supervisorForTest(t, root)
			assertFull()
			if _, err := s.ClaimMailbox("worker", ids[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`CREATE TRIGGER fail_ack BEFORE UPDATE ON supervisor_state BEGIN SELECT RAISE(FAIL, 'injected ack failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := s.AckMailbox("worker", ids[0], first.MessageID); err == nil {
				t.Fatal("ack unexpectedly persisted")
			}
			assertFull()
			if _, err := s.db.Exec(`DROP TRIGGER fail_ack`); err != nil {
				t.Fatal(err)
			}
			if err := s.AckMailbox("worker", ids[0], first.MessageID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Enqueue("admin", target, "overflow", payload); err != nil {
				t.Fatal("ack did not release capacity", err)
			}
			assertFull()
			if err := s.RetireApplication("admin", ids[0]); err != nil {
				t.Fatal(err)
			}
			capacity, err := s.InspectMailboxCapacity(ids[0])
			if err != nil || capacity.Application.Messages != 0 || capacity.Application.PayloadBytes != 0 {
				t.Fatal("retirement retained capacity", capacity, err)
			}
			if _, err := s.Enqueue("admin", ids[tc.apps], "after-retire", payload); err != nil {
				t.Fatal("retirement did not release aggregate capacity", err)
			}
		})
	}
}

func TestMailboxCapacityStableJSONEncoding(t *testing.T) {
	root := t.TempDir()
	s := supervisorForTest(t, root)
	a := wakeApplication(t, s, "manual")
	payload := json.RawMessage(` { "text" : "<>&" } `)
	m, err := s.Enqueue("admin", a.ApplicationID, "encoded", payload)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.InspectMailboxCapacity(a.ApplicationID)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = supervisorForTest(t, root)
	after, _ := s.InspectMailboxCapacity(a.ApplicationID)
	if before != after {
		t.Fatalf("payload accounting changed after restart: before=%+v after=%+v", before, after)
	}
	if duplicate, err := s.Enqueue("admin", a.ApplicationID, "encoded", payload); err != nil || duplicate.MessageID != m.MessageID {
		t.Fatal("JSON serialization broke idempotent retry", err)
	}
}

func TestTimerEscapedPayloadRecovery(t *testing.T) {
	root := t.TempDir()
	s := supervisorForTest(t, root)
	a := wakeApplication(t, s, "manual")
	payload := json.RawMessage(`"` + strings.Repeat("<", maxTextBytes-2) + `"`)
	if err := s.AddTimer("admin", a.ApplicationID, "escaped", time.Nanosecond, 0, payload); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = supervisorForTest(t, root)
	if err := s.deliverTimers(); err != nil {
		t.Fatal("persisted JSON expansion prevented timer delivery", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	capacity, _ := s.InspectMailboxCapacity(a.ApplicationID)
	if capacity.Application.PayloadBytes != int64(len(encoded)) || capacity.Application.Messages != 1 {
		t.Fatal("timer did not charge persisted JSON size", capacity)
	}
}

func TestLSFSWakeBackpressureReplay(t *testing.T) {
	root := t.TempDir()
	s := supervisorForTest(t, root)
	a := wakeApplication(t, s, "manual")
	first := seedMailbox(t, s, []string{a.ApplicationID}, maxApplicationMailboxMessages-1, json.RawMessage(`{}`))[0]
	importWakeArtifact(t, s, a.ApplicationID, "first")
	secondHandle := importWakeArtifact(t, s, a.ApplicationID, "second")
	if err := s.deliverLSFS(); err != nil {
		t.Fatal("capacity stopped source delivery", err)
	}
	bindings, _ := s.ListLSFSBindings(a.ApplicationID)
	commits, err := s.contexts.Commits(a.OwnerAgent, a.ContextID, 0, 100)
	if err != nil || len(commits) != 2 || bindings[0].Cursor != commits[0].Cursor {
		t.Fatal("cursor passed an undelivered commit", bindings, commits, err)
	}
	// A full manual mailbox must not stop the runner from serving another app.
	b, err := s.CreateApplication("admin", "other", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Enqueue("admin", b.ApplicationID, "run", json.RawMessage(`{"text":"progress"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, EchoProvider{}) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err = s.Result(b.ApplicationID, m.MessageID); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if runErr := <-done; runErr != nil || err != nil {
		t.Fatalf("backpressure stopped unrelated work: run=%v result=%v", runErr, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = supervisorForTest(t, root)
	if err := s.deliverLSFS(); err != nil {
		t.Fatal(err)
	}
	recovered, _ := s.ListLSFSBindings(a.ApplicationID)
	if recovered[0].Cursor != bindings[0].Cursor {
		t.Fatal("restart advanced a blocked cursor")
	}
	if _, err := s.ClaimMailbox("worker", a.ApplicationID); err != nil {
		t.Fatal(err)
	}
	if err := s.AckMailbox("worker", a.ApplicationID, first.MessageID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.deliverLSFS(); err != nil {
			t.Fatal(err)
		}
	}
	mailbox, err := s.ListMailbox(a.ApplicationID)
	if err != nil || len(mailbox) != maxApplicationMailboxMessages+1 {
		t.Fatal("lost or duplicated deferred event", len(mailbox), err)
	}
	var event LSFSWakeEvent
	if err := json.Unmarshal(mailbox[len(mailbox)-1].Payload, &event); err != nil || event.Commit.Handle != secondHandle {
		t.Fatal("wrong deferred event", event, err)
	}
	capacity, _ := s.InspectMailboxCapacity(a.ApplicationID)
	if capacity.Application.Messages != maxApplicationMailboxMessages {
		t.Fatal("replay escaped capacity", capacity)
	}
}

func TestTimerBackpressureRestartAndRollback(t *testing.T) {
	for _, interval := range []time.Duration{0, time.Hour} {
		t.Run(interval.String(), func(t *testing.T) {
			root := t.TempDir()
			s := supervisorForTest(t, root)
			a := wakeApplication(t, s, "manual")
			first := seedMailbox(t, s, []string{a.ApplicationID}, maxApplicationMailboxMessages, json.RawMessage(`{}`))[0]
			if err := s.AddTimer("admin", a.ApplicationID, "tick", time.Nanosecond, interval, json.RawMessage(`{"tick":true}`)); err != nil {
				t.Fatal(err)
			}
			before, _ := s.ListTimers(a.ApplicationID)
			if err := s.deliverTimers(); err != nil {
				t.Fatal(err)
			}
			after, _ := s.ListTimers(a.ApplicationID)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("blocked timer changed due or disappeared")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s = supervisorForTest(t, root)
			if _, err := s.ClaimMailbox("worker", a.ApplicationID); err != nil {
				t.Fatal(err)
			}
			if err := s.AckMailbox("worker", a.ApplicationID, first.MessageID); err != nil {
				t.Fatal(err)
			}
			committed := string(s.committed)
			if _, err := s.db.Exec(`CREATE TRIGGER fail_timer BEFORE UPDATE ON supervisor_state BEGIN SELECT RAISE(FAIL, 'injected timer failure'); END`); err != nil {
				t.Fatal(err)
			}
			if err := s.deliverTimers(); err == nil {
				t.Fatal("timer committed despite persistence failure")
			}
			state, err := json.Marshal(s.state)
			if err != nil || string(state) != committed {
				t.Fatal("timer/mailbox transaction was not rolled back", err)
			}
			if _, err := s.db.Exec(`DROP TRIGGER fail_timer`); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := s.deliverTimers(); err != nil {
					t.Fatal(err)
				}
			}
			mailbox, err := s.ListMailbox(a.ApplicationID)
			if err != nil || len(mailbox) != maxApplicationMailboxMessages+1 || mailbox[len(mailbox)-1].IdempotencyKey != fmt.Sprintf("timer:tick:%d", before[0].Due) {
				t.Fatal("timer replay lost original identity or duplicated", len(mailbox), err)
			}
			after, _ = s.ListTimers(a.ApplicationID)
			if interval == 0 && len(after) != 0 || interval > 0 && (len(after) != 1 || after[0].Due <= before[0].Due) {
				t.Fatal("delivered timer schedule not committed", after)
			}
		})
	}
}
