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
	nextEvent         map[uint64]uint64
	eagainNext        bool
}

func newFakeKernelBridge() *fakeKernelBridge {
	return &fakeKernelBridge{queues: map[uint64][]KernelEvent{}, nextEvent: map[uint64]uint64{}}
}

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
	a.bridge.mu.Lock()
	defer a.bridge.mu.Unlock()
	a.bridge.restoreAttempts++
	if len(a.bridge.queues[a.id]) != 0 {
		a.bridge.restoreRejections++
		return errors.New("fake EBUSY: queue not empty")
	}
	a.bridge.queues[a.id] = append([]KernelEvent(nil), events...)
	return nil
}

func (a *fakeKernelApp) Close() error { return nil }

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
	if mailbox[0].IdempotencyKey != fmt.Sprintf("kernel:%s:1", app.ApplicationID) {
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
	// Fresh boot: the kernel registry starts empty and the persisted
	// snapshot from the previous VM is restored into it.
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
	if pending := boot.pending(app.KernelID); len(pending) != 2 {
		t.Fatalf("restore rebuilt %d events, want 2", len(pending))
	}
	if err := s2.deliverKernel(); err != nil {
		t.Fatalf("deliverKernel: %v", err)
	}
	mailbox, _ := s2.ListMailbox(app.ApplicationID)
	if len(mailbox) != 2 || mailbox[0].IdempotencyKey != fmt.Sprintf("kernel:%s:7", app.ApplicationID) {
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
