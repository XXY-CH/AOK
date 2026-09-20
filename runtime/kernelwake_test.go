package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// fakeKernelBridge mirrors the kernel registry: queues survive handles,
// cursors are per-open, restore requires an empty queue.
type fakeKernelBridge struct {
	mu                sync.Mutex
	queues            map[uint64][]KernelEvent
	ackLog            []uint64
	restoreAttempts   int
	restoreRejections int
	restoreErr        error
	beforeRestore     func(*fakeKernelBridge, uint64)
	nextEvent         map[uint64]uint64
	eagainNext        bool
	ackFail           bool
}

func newFakeKernelBridge() *fakeKernelBridge {
	return &fakeKernelBridge{queues: map[uint64][]KernelEvent{}, nextEvent: map[uint64]uint64{}}
}

func (b *fakeKernelBridge) BootID() (string, error) { return fmt.Sprintf("boot-%p", b), nil }

func (b *fakeKernelBridge) post(id uint64, events ...KernelEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.queues[id] = append(b.queues[id], events...)
}

func (b *fakeKernelBridge) pending(id uint64) []KernelEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]KernelEvent(nil), b.queues[id]...)
}

func (b *fakeKernelBridge) Open(id uint64) (KernelApplication, error) {
	return &fakeKernelApp{bridge: b, id: id}, nil
}

func (b *fakeKernelBridge) OpenSource(kind uint32) (KernelEventSource, error) {
	if kind != KernelSourceLSFS {
		return nil, errors.New("fake bridge only produces LSFS sources")
	}
	return &fakeKernelSource{bridge: b}, nil
}

type fakeKernelSource struct {
	bridge   *fakeKernelBridge
	appID    uint64
	attached int
}

func (s *fakeKernelSource) Attach(_ KernelApplication, applicationID uint64) error {
	s.appID = applicationID
	s.attached++
	return nil
}

func (s *fakeKernelSource) Post(cursor uint64) error {
	s.bridge.mu.Lock()
	defer s.bridge.mu.Unlock()
	if s.bridge.eagainNext || len(s.bridge.queues[s.appID]) >= 128 {
		return syscall.EAGAIN
	}
	s.bridge.nextEvent[s.appID]++
	seq := s.bridge.nextEvent[s.appID]
	s.bridge.queues[s.appID] = append(s.bridge.queues[s.appID], KernelEvent{
		ApplicationID: s.appID, EventID: seq, EventSeq: seq,
		Kind: KernelSourceLSFS, Data: cursor})
	return nil
}

func (s *fakeKernelSource) Close() error { return nil }

type fakeKernelApp struct {
	bridge  *fakeKernelBridge
	id      uint64
	mu      sync.Mutex
	readSeq uint64
}

func (a *fakeKernelApp) Read() (*KernelEvent, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bridge.mu.Lock()
	defer a.bridge.mu.Unlock()
	for _, ev := range a.bridge.queues[a.id] {
		if ev.EventSeq > a.readSeq {
			a.readSeq = ev.EventSeq
			out := ev
			return &out, nil
		}
	}
	return nil, nil
}

func (a *fakeKernelApp) Ack(eventID uint64) error {
	a.bridge.mu.Lock()
	defer a.bridge.mu.Unlock()
	if a.bridge.ackFail {
		return errors.New("injected ACK failure")
	}
	queue := a.bridge.queues[a.id]
	for i, ev := range queue {
		if ev.EventID != eventID {
			continue
		}
		a.bridge.queues[a.id] = append(queue[:i:i], queue[i+1:]...)
		a.bridge.ackLog = append(a.bridge.ackLog, eventID)
		return nil
	}
	return nil
}

func (a *fakeKernelApp) Snapshot() ([]KernelEvent, error) {
	return a.bridge.pending(a.id), nil
}

func (a *fakeKernelApp) Restore(events []KernelEvent) error {
	if a.bridge.beforeRestore != nil {
		hook := a.bridge.beforeRestore
		a.bridge.beforeRestore = nil
		hook(a.bridge, a.id)
	}
	a.bridge.mu.Lock()
	defer a.bridge.mu.Unlock()
	a.bridge.restoreAttempts++
	if a.bridge.restoreErr != nil {
		return a.bridge.restoreErr
	}
	if len(a.bridge.queues[a.id]) != 0 {
		a.bridge.restoreRejections++
		return syscall.EBUSY
	}
	a.bridge.queues[a.id] = append([]KernelEvent(nil), events...)
	for _, event := range events {
		if event.EventID > a.bridge.nextEvent[a.id] {
			a.bridge.nextEvent[a.id] = event.EventID
		}
	}
	return nil
}

func (a *fakeKernelApp) Close() error { return nil }

func TestKernelEventIdentitySurvivesRebootAndAckFailure(t *testing.T) {
	for _, ackFailure := range []bool{false, true} {
		for _, sameBoot := range []bool{false, true} {
			t.Run(fmt.Sprintf("ack_failure=%v/same_boot=%v", ackFailure, sameBoot), func(t *testing.T) {
				root := t.TempDir()
				if err := os.Chmod(root, 0700); err != nil {
					t.Fatal(err)
				}
				s, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
				if err != nil {
					t.Fatal(err)
				}
				app, err := s.CreateApplication("tester", "owner", "on_event")
				if err != nil {
					t.Fatal(err)
				}
				old := newFakeKernelBridge()
				old.ackFail = ackFailure
				old.post(app.KernelID, KernelEvent{ApplicationID: app.KernelID, EventID: 1, EventSeq: 1, Kind: 3, Data: 100})
				s.SetKernelBridge(old)
				if err := s.deliverKernel(); (err != nil) != ackFailure {
					t.Fatalf("delivery: %v", err)
				}
				// Skip orderly snapshot: only committed delivery state survives.
				s.SetKernelBridge(nil)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				bridge := newFakeKernelBridge()
				if sameBoot {
					bridge = old
					bridge.ackFail = false
				}
				s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
				if err != nil {
					t.Fatal(err)
				}
				defer s2.Close()
				s2.SetKernelBridge(bridge)
				if err := s2.attachKernel(); err != nil {
					t.Fatal(err)
				}
				if err := s2.deliverKernel(); err != nil {
					t.Fatal(err)
				}
				mailbox, _ := s2.ListMailbox(app.ApplicationID)
				if len(mailbox) != 1 {
					t.Fatalf("recovery duplicated delivery: %d", len(mailbox))
				}
				seq := uint64(1)
				if sameBoot || ackFailure {
					seq = 2
				}
				bridge.post(app.KernelID, KernelEvent{ApplicationID: app.KernelID, EventID: seq, EventSeq: seq, Kind: 3, Data: 200})
				if err := s2.deliverKernel(); err != nil {
					t.Fatal(err)
				}
				mailbox, _ = s2.ListMailbox(app.ApplicationID)
				if len(mailbox) != 2 || mailbox[0].IdempotencyKey == mailbox[1].IdempotencyKey {
					t.Fatal("new event collided with previous boot")
				}
			})
		}
	}
}

func TestRollbackRestoresOptionalMaps(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	s.sourced["uncommitted"] = "stale"
	s.restoreLocked()
	if s.state.KernelPending == nil || s.state.Channels == nil || s.state.Conversations == nil ||
		s.state.HostfsMounts == nil || s.state.Confirmations == nil || s.state.Bindings == nil {
		t.Fatal("rollback left optional maps nil")
	}
	if len(s.sourced) != 0 {
		t.Fatal("rollback kept an uncommitted source")
	}
	bridge := newFakeKernelBridge()
	bridge.post(app.KernelID, KernelEvent{EventID: 1, EventSeq: 1, Kind: 3})
	s.SetKernelBridge(bridge)
	s.snapshotKernel()
	if len(s.state.KernelPending[app.ApplicationID]) != 1 {
		t.Fatal("snapshot after rollback lost pending event")
	}
}

func newKernelTestSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestKernelRegistryDrainAndAck(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if app.KernelID == 0 {
		t.Fatal("application needs a kernel id")
	}
	bridge := newFakeKernelBridge()
	bridge.post(app.KernelID,
		KernelEvent{ApplicationID: app.KernelID, EventID: 1, EventSeq: 1, Kind: 3, Data: 5},
		KernelEvent{ApplicationID: app.KernelID, EventID: 2, EventSeq: 2, Kind: 1})
	s.SetKernelBridge(bridge)
	if err := s.attachKernel(); err != nil {
		t.Fatalf("attachKernel: %v", err)
	}
	if err := s.deliverKernel(); err != nil {
		t.Fatalf("deliverKernel: %v", err)
	}
	mailbox, err := s.ListMailbox(app.ApplicationID)
	if err != nil {
		t.Fatalf("ListMailbox: %v", err)
	}
	if len(mailbox) != 2 {
		t.Fatalf("mailbox has %d messages, want 2", len(mailbox))
	}
	boot, _ := bridge.BootID()
	if mailbox[0].IdempotencyKey != fmt.Sprintf("kernel:%s:%s:1", app.ApplicationID, boot) {
		t.Fatalf("unexpected idempotency key %s", mailbox[0].IdempotencyKey)
	}
	var event KernelWakeEvent
	if err := json.Unmarshal(mailbox[0].Payload, &event); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if event.SourceKind != "kernel" || event.KernelKind != "lsfs" ||
		event.Data != 5 || event.ApplicationID != app.ApplicationID {
		t.Fatalf("unexpected payload %+v", event)
	}
	if pending := bridge.pending(app.KernelID); len(pending) != 0 {
		t.Fatalf("kernel queue kept %d events after ack", len(pending))
	}
	if err := s.deliverKernel(); err != nil {
		t.Fatalf("second deliverKernel: %v", err)
	}
	mailbox, _ = s.ListMailbox(app.ApplicationID)
	if len(mailbox) != 2 {
		t.Fatalf("re-drain duplicated events: %d messages", len(mailbox))
	}
}

func TestKernelRegistryBackpressureKeepsEvents(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	for i := 0; i < maxApplicationMailboxMessages; i++ {
		if _, err = s.Enqueue("tester", app.ApplicationID,
			fmt.Sprintf("bulk-%d", i), json.RawMessage(`{"text":"x"}`)); err != nil {
			t.Fatalf("bulk enqueue %d: %v", i, err)
		}
	}
	bridge := newFakeKernelBridge()
	bridge.post(app.KernelID,
		KernelEvent{EventID: 1, EventSeq: 1, Kind: 3, Data: 1},
		KernelEvent{EventID: 2, EventSeq: 2, Kind: 3, Data: 2})
	s.SetKernelBridge(bridge)
	if err := s.attachKernel(); err != nil {
		t.Fatalf("attachKernel: %v", err)
	}
	if err := s.deliverKernel(); err != nil {
		t.Fatalf("deliverKernel under backpressure: %v", err)
	}
	if pending := bridge.pending(app.KernelID); len(pending) != 2 {
		t.Fatalf("backpressure must keep kernel events, got %d", len(pending))
	}
	claimed, err := s.ClaimMailbox("tester", app.ApplicationID)
	if err != nil {
		t.Fatalf("ClaimMailbox: %v", err)
	}
	for _, m := range claimed {
		if err := s.AckMailbox("tester", app.ApplicationID, m.MessageID); err != nil {
			t.Fatalf("AckMailbox: %v", err)
		}
	}
	if err := s.deliverKernel(); err != nil {
		t.Fatalf("deliverKernel after release: %v", err)
	}
	if pending := bridge.pending(app.KernelID); len(pending) != 0 {
		t.Fatalf("kernel events not drained after release: %d", len(pending))
	}
	mailbox, _ := s.ListMailbox(app.ApplicationID)
	delivered := 0
	for _, m := range mailbox {
		if strings.HasPrefix(m.IdempotencyKey, "kernel:") &&
			(m.Status == "pending" || m.Status == "claimed") {
			delivered++
		}
	}
	if delivered != 2 {
		t.Fatalf("expected 2 delivered kernel events, got %d", delivered)
	}
}

func TestKernelRegistryShutdownSnapshotAndRestart(t *testing.T) {
	// Fresh boot: persisted records are delivered before the new kernel
	// queue is consumed, preserving their original boot identity.
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	bridge := newFakeKernelBridge()
	bridge.post(app.KernelID,
		KernelEvent{EventID: 7, EventSeq: 7, Kind: 3, Data: 70},
		KernelEvent{EventID: 8, EventSeq: 8, Kind: 2})
	s.SetKernelBridge(bridge)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	boot := newFakeKernelBridge()
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	s2.SetKernelBridge(boot)
	if err := s2.attachKernel(); err != nil {
		t.Fatalf("attachKernel: %v", err)
	}
	if pending := boot.pending(app.KernelID); len(pending) != 0 {
		t.Fatalf("fresh boot unexpectedly restored %d events", len(pending))
	}
	if boot.restoreAttempts != 0 {
		t.Fatalf("cross-boot recovery attempted Restore %d times", boot.restoreAttempts)
	}
	if err := s2.deliverKernel(); err != nil {
		t.Fatalf("deliverKernel: %v", err)
	}
	mailbox, _ := s2.ListMailbox(app.ApplicationID)
	origin, _ := bridge.BootID()
	if len(mailbox) != 2 || mailbox[0].IdempotencyKey != fmt.Sprintf("kernel:%s:%s:7", app.ApplicationID, origin) {
		t.Fatalf("restored events not delivered once: %+v", mailbox)
	}

	// Same-boot restart: the kernel queue survives the supervisor, so
	// the persisted copy is discarded instead of duplicated and the live
	// queue re-drains through the same idempotent keys.
	root2 := t.TempDir()
	if err := os.Chmod(root2, 0700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s3, err := NewSupervisor(root2, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	app3, err := s3.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	live := newFakeKernelBridge()
	live.post(app3.KernelID,
		KernelEvent{EventID: 7, EventSeq: 7, Kind: 3, Data: 70},
		KernelEvent{EventID: 8, EventSeq: 8, Kind: 2})
	s3.SetKernelBridge(live)
	if err := s3.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s4, err := NewSupervisor(root2, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatalf("reopen same boot: %v", err)
	}
	defer s4.Close()
	s4.SetKernelBridge(live)
	if err := s4.attachKernel(); err != nil {
		t.Fatalf("attachKernel same boot: %v", err)
	}
	if live.restoreRejections != 1 {
		t.Fatalf("expected one rejected restore, got %d", live.restoreRejections)
	}
	if err := s4.deliverKernel(); err != nil {
		t.Fatalf("deliverKernel same boot: %v", err)
	}
	mailbox, _ = s4.ListMailbox(app3.ApplicationID)
	if len(mailbox) != 2 {
		t.Fatalf("same-boot re-drain duplicated events: %d", len(mailbox))
	}
}

func TestKernelRecoveryAvoidsCrossBootRestoreRaceAndIDCollision(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	old := newFakeKernelBridge()
	old.post(app.KernelID, KernelEvent{ApplicationID: app.KernelID, EventID: 1, EventSeq: 1, Kind: 3, Data: 10})
	s.SetKernelBridge(old)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	current := newFakeKernelBridge()
	current.restoreErr = syscall.EINVAL
	current.post(app.KernelID, KernelEvent{ApplicationID: app.KernelID, EventID: 1, EventSeq: 1, Kind: 3, Data: 20})
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.SetKernelBridge(current)
	if err := s2.attachKernel(); err != nil {
		t.Fatal(err)
	}
	if current.restoreAttempts != 0 {
		t.Fatal("cross-boot recovery must not race a producer through Restore")
	}
	if err := s2.deliverKernel(); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := s2.ListMailbox(app.ApplicationID)
	if len(mailbox) != 2 || mailbox[0].IdempotencyKey == mailbox[1].IdempotencyKey {
		t.Fatalf("cross-boot event-id collision lost an event: %+v", mailbox)
	}
	var first, second KernelWakeEvent
	if err := json.Unmarshal(mailbox[0].Payload, &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mailbox[1].Payload, &second); err != nil {
		t.Fatal(err)
	}
	if first.Data != 10 || second.Data != 20 {
		t.Fatalf("wrong cross-boot deliveries: %+v %+v", first, second)
	}
}

func TestKernelSameBootRestoreRaceAndEINVAL(t *testing.T) {
	for _, tc := range []struct {
		name       string
		restoreErr error
		wantErr    bool
	}{
		{name: "producer wins race"},
		{name: "EINVAL is returned", restoreErr: syscall.EINVAL, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			s, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
			if err != nil {
				t.Fatal(err)
			}
			app, err := s.CreateApplication("tester", "owner", "on_event")
			if err != nil {
				t.Fatal(err)
			}
			bridge := newFakeKernelBridge()
			bridge.post(app.KernelID, KernelEvent{ApplicationID: app.KernelID, EventID: 1, EventSeq: 1, Kind: 3, Data: 10})
			s.SetKernelBridge(bridge)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			bridge.mu.Lock()
			bridge.queues[app.KernelID] = nil
			bridge.mu.Unlock()
			bridge.restoreErr = tc.restoreErr
			if tc.restoreErr == nil {
				bridge.beforeRestore = func(b *fakeKernelBridge, id uint64) {
					b.post(id, KernelEvent{ApplicationID: id, EventID: 2, EventSeq: 2, Kind: 3, Data: 20})
				}
			}
			s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
			if err != nil {
				t.Fatal(err)
			}
			defer s2.Close()
			s2.SetKernelBridge(bridge)
			err = s2.attachKernel()
			if tc.wantErr {
				if !errors.Is(err, syscall.EINVAL) {
					t.Fatalf("attach swallowed EINVAL: %v", err)
				}
				mailbox, _ := s2.ListMailbox(app.ApplicationID)
				if len(mailbox) != 1 {
					t.Fatalf("old pending was not durably delivered: %+v", mailbox)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s2.deliverKernel(); err != nil {
				t.Fatal(err)
			}
			mailbox, _ := s2.ListMailbox(app.ApplicationID)
			if len(mailbox) != 2 {
				t.Fatalf("Restore race lost old or live event: %+v", mailbox)
			}
		})
	}
}

func TestKernelFailedRecoveryClosePreservesPending(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	old := newFakeKernelBridge()
	old.post(app.KernelID, KernelEvent{ApplicationID: app.KernelID, EventID: 7, EventSeq: 7, Kind: 3, Data: 70})
	s.SetKernelBridge(old)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	current := newFakeKernelBridge()
	s2.SetKernelBridge(current)
	if _, err := s2.db.Exec(`CREATE TRIGGER fail_kernel_recovery BEFORE UPDATE ON supervisor_state BEGIN SELECT RAISE(FAIL, 'injected kernel recovery failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s2.attachKernel(); err == nil {
		t.Fatal("attach succeeded despite persistence failure")
	}
	if _, err := s2.db.Exec(`DROP TRIGGER fail_kernel_recovery`); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if pending := s3.state.KernelPending[app.ApplicationID]; len(pending) != 1 || pending[0].Data != 70 {
		t.Fatalf("failed attach/Close lost original pending: %+v", pending)
	}
}

func TestKernelRecoveryResolvesCommittedDeliveryIntent(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	old := newFakeKernelBridge()
	old.post(app.KernelID, KernelEvent{ApplicationID: app.KernelID, EventID: 1, EventSeq: 1, Kind: 3, Data: 10})
	s.SetKernelBridge(old)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Model a crash-era state in which the old event reached the mailbox but
	// its pending record was still present. Recovery must resolve it by the
	// original identity without classifying a colliding new-boot event as old.
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	s2.mu.Lock()
	event := s2.state.KernelPending[app.ApplicationID][0]
	payload, err := kernelWakePayload(app.ApplicationID, event)
	if err == nil {
		_, err = s2.enqueueLocked("supervisor", app.ApplicationID,
			kernelMessageKey(app.ApplicationID, event), payload)
	}
	if err == nil {
		err = s2.persistLocked(nil)
	}
	s2.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	s2.SetKernelBridge(nil)
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}

	current := newFakeKernelBridge()
	current.post(app.KernelID, KernelEvent{ApplicationID: app.KernelID, EventID: 1, EventSeq: 1, Kind: 3, Data: 20})
	s3, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	s3.SetKernelBridge(current)
	if err := s3.attachKernel(); err != nil {
		t.Fatal(err)
	}
	if err := s3.deliverKernel(); err != nil {
		t.Fatal(err)
	}
	mailbox, _ := s3.ListMailbox(app.ApplicationID)
	if len(mailbox) != 2 || mailbox[0].IdempotencyKey == mailbox[1].IdempotencyKey {
		t.Fatalf("committed recovery intent conflated boot identities: %+v", mailbox)
	}
}

func TestKernelLegacyPendingMigrationAfterVMRestart(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprintf("already_delivered=%v", delivered), func(t *testing.T) {
			s := newKernelTestSupervisor(t)
			app, err := s.CreateApplication("tester", "owner", "on_event")
			if err != nil {
				t.Fatal(err)
			}
			legacy := KernelEvent{ApplicationID: app.KernelID, EventID: 1, EventSeq: 1, Kind: 3, Data: 10}
			s.state.KernelPending[app.ApplicationID] = []KernelEvent{legacy}
			if delivered {
				payload, err := kernelWakePayload(app.ApplicationID, legacy)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := s.enqueueLocked("supervisor", app.ApplicationID, kernelMessageKey(app.ApplicationID, legacy), payload); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.persistLocked(nil); err != nil {
				t.Fatal(err)
			}
			s.restoreLocked()
			// A new VM can produce an identical record with the same numeric id.
			current := newFakeKernelBridge()
			current.post(app.KernelID, legacy)
			s.SetKernelBridge(current)
			if err := s.attachKernel(); err != nil {
				t.Fatal(err)
			}
			if err := s.deliverKernel(); err != nil {
				t.Fatal(err)
			}
			mailbox, err := s.ListMailbox(app.ApplicationID)
			if err != nil {
				t.Fatal(err)
			}
			if len(mailbox) != 2 || mailbox[0].IdempotencyKey != kernelMessageKey(app.ApplicationID, legacy) ||
				mailbox[0].IdempotencyKey == mailbox[1].IdempotencyKey {
				t.Fatalf("legacy recovery conflated the new boot event: %+v", mailbox)
			}
			if current.restoreAttempts != 0 || len(current.pending(app.KernelID)) != 0 {
				t.Fatal("legacy recovery must drain the new queue without restoring old events into it")
			}
		})
	}
}

func TestKernelIdempotencyKeyIsReserved(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if _, err := s.Enqueue("tester", app.ApplicationID,
		"kernel:x:1", json.RawMessage(`{}`)); err == nil {
		t.Fatal("kernel idempotency key must be reserved")
	}
}

func TestKernelIDAssignmentPersists(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	first, _ := s.CreateApplication("tester", "a", "on_event")
	second, _ := s.CreateApplication("tester", "b", "on_event")
	if first.KernelID == 0 || second.KernelID != first.KernelID+1 {
		t.Fatalf("unexpected kernel ids %d %d", first.KernelID, second.KernelID)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	again, err := s2.InspectApplication(first.ApplicationID)
	if err != nil || again.KernelID != first.KernelID {
		t.Fatalf("kernel id not preserved: %+v err=%v", again, err)
	}
	third, err := s2.CreateApplication("tester", "c", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if third.KernelID <= second.KernelID {
		t.Fatalf("new kernel id %d must exceed %d", third.KernelID, second.KernelID)
	}
}

func TestKernelLSFSBindingsRouteThroughKernel(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	bridge := newFakeKernelBridge()
	s.SetKernelBridge(bridge)
	if err := s.AddLSFSBinding("tester", app.ApplicationID, "b1", "", 0); err != nil {
		t.Fatalf("AddLSFSBinding: %v", err)
	}
	if _, err := s.ImportArtifact("tester", app.ApplicationID, "k1",
		json.RawMessage(`{"v":1}`)); err != nil {
		t.Fatalf("ImportArtifact: %v", err)
	}
	if err := s.deliverLSFS(); err != nil {
		t.Fatalf("deliverLSFS: %v", err)
	}
	pending := bridge.pending(app.KernelID)
	if len(pending) != 1 || pending[0].Kind != KernelSourceLSFS ||
		pending[0].Data == 0 {
		t.Fatalf("artifact commit not posted to kernel queue: %+v", pending)
	}
	if mailbox, _ := s.ListMailbox(app.ApplicationID); len(mailbox) != 0 {
		t.Fatalf("kernel-routed commit must not enqueue directly: %+v", mailbox)
	}
	if err := s.deliverKernel(); err != nil {
		t.Fatalf("deliverKernel: %v", err)
	}
	mailbox, err := s.ListMailbox(app.ApplicationID)
	if err != nil || len(mailbox) != 1 ||
		!strings.HasPrefix(mailbox[0].IdempotencyKey, "kernel:") {
		t.Fatalf("drained kernel event missing: %+v err=%v", mailbox, err)
	}
	var event KernelWakeEvent
	if err := json.Unmarshal(mailbox[0].Payload, &event); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if event.KernelKind != "lsfs" || event.Data != pending[0].Data {
		t.Fatalf("cursor not preserved: %+v", event)
	}
	if left := bridge.pending(app.KernelID); len(left) != 0 {
		t.Fatalf("kernel queue not drained: %+v", left)
	}
}

func TestKernelLSFSBackpressureKeepsCursor(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	bridge := newFakeKernelBridge()
	bridge.eagainNext = true
	s.SetKernelBridge(bridge)
	if err := s.AddLSFSBinding("tester", app.ApplicationID, "b1", "", 0); err != nil {
		t.Fatalf("AddLSFSBinding: %v", err)
	}
	if _, err := s.ImportArtifact("tester", app.ApplicationID, "k1",
		json.RawMessage(`{}`)); err != nil {
		t.Fatalf("ImportArtifact: %v", err)
	}
	if err := s.deliverLSFS(); err != nil {
		t.Fatalf("deliverLSFS under backpressure: %v", err)
	}
	if pending := bridge.pending(app.KernelID); len(pending) != 0 {
		t.Fatalf("EAGAIN must not enqueue: %+v", pending)
	}
	bindings, err := s.ListLSFSBindings(app.ApplicationID)
	if err != nil || len(bindings) != 1 || bindings[0].Cursor != 0 {
		t.Fatalf("cursor advanced past refused post: %+v err=%v", bindings, err)
	}
	bridge.eagainNext = false
	if err := s.deliverLSFS(); err != nil {
		t.Fatalf("deliverLSFS after release: %v", err)
	}
	if pending := bridge.pending(app.KernelID); len(pending) != 1 {
		t.Fatalf("post not retried: %+v", pending)
	}
	bindings, _ = s.ListLSFSBindings(app.ApplicationID)
	if bindings[0].Cursor == 0 {
		t.Fatal("cursor did not advance after successful post")
	}
}
