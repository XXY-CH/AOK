package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"syscall"
)

// KernelEvent is the durable-queue record of the kernel application registry.
type KernelEvent struct {
	BootID      string `json:"boot_id,omitempty"`
	QueueBootID string `json:"queue_boot_id,omitempty"`
	// RecoveryPending is process-local. It prevents a failed recovery attempt
	// from letting the shutdown snapshot overwrite the durable pending copy.
	RecoveryPending bool   `json:"-"`
	ApplicationID   uint64 `json:"application_id"`
	EventID         uint64 `json:"event_id"`
	EventSeq        uint64 `json:"event_seq"`
	Kind            uint32 `json:"kind"`
	Coalesced       uint32 `json:"coalesced"`
	Data            uint64 `json:"data"`
}

// KernelApplication is one registered application's durable kernel queue.
type KernelApplication interface {
	// Read returns the next unacked event from a private cursor, nil at end.
	Read() (*KernelEvent, error)
	// Ack retires one event; it must only run after the delivery is durable.
	Ack(eventID uint64) error
	Snapshot() ([]KernelEvent, error)
	Restore(events []KernelEvent) error
	Close() error
}

// KernelEventSource is one kernel event producer attached to an application.
type KernelEventSource interface {
	// Attach routes every event the source fires into the application queue.
	Attach(app KernelApplication, applicationID uint64) error
	// Post enqueues one event carrying the cursor; a full durable queue
	// returns EAGAIN without consuming an event id and the caller retries.
	Post(cursor uint64) error
	Close() error
}

// KernelEventBridge opens kernel registry handles by numeric application id.
// Handles are idempotent within one boot, which is the cross-process
// recovery path after a supervisor restart.
type KernelEventBridge interface {
	// BootID is stable across supervisor restarts, distinct across VM boots.
	BootID() (string, error)
	Open(applicationID uint64) (KernelApplication, error)
	// OpenSource creates a producer of the given KernelSource* kind.
	OpenSource(kind uint32) (KernelEventSource, error)
}

// Kernel source kinds mirror the UAPI AOK_EVENT_SOURCE_* values.
const (
	KernelSourceTimer uint32 = 1
	KernelSourcePort  uint32 = 2
	KernelSourceLSFS  uint32 = 3
)

// KernelWakeEvent is the mailbox payload for one drained kernel event.
type KernelWakeEvent struct {
	SourceKind    string `json:"source_kind"`
	ApplicationID string `json:"application_id"`
	EventID       uint64 `json:"event_id"`
	EventSeq      uint64 `json:"event_seq"`
	KernelKind    string `json:"kernel_kind"`
	Coalesced     uint32 `json:"coalesced"`
	Data          uint64 `json:"data"`
	Text          string `json:"text"`
}

func kernelKindName(kind uint32) string {
	switch kind {
	case 1:
		return "timer"
	case 2:
		return "port"
	case 3:
		return "lsfs"
	}
	return "unknown"
}

// SetKernelBridge connects the kernel application registry. It must run
// before Run; without a bridge the supervisor stays purely user-space.
func (s *Supervisor) SetKernelBridge(bridge KernelEventBridge) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeKernelHandlesLocked()
	if s.kernelApps == nil {
		s.kernelApps = map[string]KernelApplication{}
	}
	if s.kernelSrcs == nil {
		s.kernelSrcs = map[string]KernelEventSource{}
	}
	s.kernel = bridge
}

func (s *Supervisor) closeKernelHandlesLocked() {
	for id, h := range s.kernelApps {
		h.Close()
		delete(s.kernelApps, id)
	}
	for id, src := range s.kernelSrcs {
		src.Close()
		delete(s.kernelSrcs, id)
	}
}

// ensureKernelSourceLocked opens and attaches the application's LSFS
// producer; the binding scan posts artifact commits into the kernel durable
// queue instead of enqueueing directly.
func (s *Supervisor) ensureKernelSourceLocked(id string) (KernelEventSource, error) {
	if src, ok := s.kernelSrcs[id]; ok {
		return src, nil
	}
	app, err := s.ensureKernelHandleLocked(id)
	if err != nil {
		return nil, err
	}
	src, err := s.kernel.OpenSource(KernelSourceLSFS)
	if err != nil {
		return nil, err
	}
	kernelID := s.state.Applications[id].KernelID
	if err := src.Attach(app, kernelID); err != nil {
		src.Close()
		return nil, err
	}
	s.kernelSrcs[id] = src
	return src, nil
}

func (s *Supervisor) ensureKernelHandleLocked(id string) (KernelApplication, error) {
	if h, ok := s.kernelApps[id]; ok {
		return h, nil
	}
	if s.kernel == nil {
		return nil, errors.New("kernel event bridge is not attached")
	}
	h, err := s.kernel.Open(s.state.Applications[id].KernelID)
	if err != nil {
		return nil, err
	}
	s.kernelApps[id] = h
	return h, nil
}

// attachKernel durably delivers persisted kernel snapshots before the runner
// starts consuming the live queue. Cross-boot reinjection is unsafe here: a
// producer can fill an empty queue before Restore, and after a crash the two
// cases are indistinguishable. Same-boot queues replay through the original
// boot-scoped idempotency keys and are ACKed.
func (s *Supervisor) attachKernel() error {
	if s.kernel == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	boot, err := s.kernel.BootID()
	if err != nil || boot == "" {
		return fmt.Errorf("kernel boot identity unavailable: %v", err)
	}
	ids := make([]string, 0, len(s.state.Applications))
	for id := range s.state.Applications {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := s.state.Applications[id]
		if a == nil || a.State == "tombstoned" || len(s.state.KernelPending[id]) == 0 {
			continue
		}
		pending := append([]KernelEvent(nil), s.state.KernelPending[id]...)
		sameBoot := true
		for _, event := range pending {
			if event.QueueBootID == "" || event.QueueBootID != boot {
				sameBoot = false
				break
			}
		}
		s.markKernelRecoveryPendingLocked(id)
		recovered, err := s.deliverPersistedKernelLocked(id)
		if err != nil {
			// persistLocked rolls state back on failure; keep shutdown from
			// replacing that committed copy with an unrelated live queue.
			s.markKernelRecoveryPendingLocked(id)
			return err
		}
		if !recovered || !sameBoot {
			continue
		}
		// A same-boot queue normally still contains these records. If it was
		// drained after the last state commit, reinjection is safe because the
		// boot-scoped mailbox identities are unchanged. Cross-boot snapshots
		// never enter this path, avoiding the producer/Restore race.
		h, err := s.ensureKernelHandleLocked(id)
		if err != nil {
			return err
		}
		if err := h.Restore(pending); err != nil && !errors.Is(err, syscall.EBUSY) {
			h.Close()
			delete(s.kernelApps, id)
			return err
		}
	}
	return nil
}

func (s *Supervisor) markKernelRecoveryPendingLocked(id string) {
	pending := s.state.KernelPending[id]
	for i := range pending {
		pending[i].RecoveryPending = true
	}
	s.state.KernelPending[id] = pending
}

func kernelWakePayload(id string, event KernelEvent) ([]byte, error) {
	return json.Marshal(KernelWakeEvent{
		SourceKind: "kernel", ApplicationID: id,
		EventID: event.EventID, EventSeq: event.EventSeq,
		KernelKind: kernelKindName(event.Kind),
		Coalesced:  event.Coalesced, Data: event.Data,
		Text: fmt.Sprintf("kernel %s event: event_id=%d seq=%d data=%d",
			kernelKindName(event.Kind), event.EventID, event.EventSeq, event.Data),
	})
}

// deliverPersistedKernelLocked atomically moves the oldest persisted snapshot
// record into the mailbox. A full mailbox leaves the record for a later tick.
func (s *Supervisor) deliverPersistedKernelLocked(id string) (bool, error) {
	for len(s.state.KernelPending[id]) > 0 {
		event := s.state.KernelPending[id][0]
		payload, err := kernelWakePayload(id, event)
		if err == nil {
			_, err = s.enqueueLocked("supervisor", id, kernelMessageKey(id, event), payload)
		}
		if errors.Is(err, ErrMailboxFull) {
			return false, nil
		}
		if err != nil {
			s.restoreLocked()
			return false, err
		}
		pending := s.state.KernelPending[id][1:]
		if len(pending) == 0 {
			delete(s.state.KernelPending, id)
		} else {
			s.state.KernelPending[id] = pending
		}
		if err := s.persistLocked(nil); err != nil {
			return false, err
		}
	}
	return true, nil
}

func sameKernelEvent(a, b KernelEvent) bool {
	return a.ApplicationID == b.ApplicationID && a.EventID == b.EventID &&
		a.EventSeq == b.EventSeq && a.Kind == b.Kind && a.Coalesced == b.Coalesced && a.Data == b.Data
}

func (s *Supervisor) identifyKernelEventLocked(id string, event KernelEvent, boot string) KernelEvent {
	event.BootID, event.QueueBootID = boot, boot
	for _, saved := range s.state.KernelPending[id] {
		if saved.QueueBootID == boot && sameKernelEvent(saved, event) {
			event.BootID = saved.BootID
			break
		}
	}
	return event
}

func kernelMessageKey(id string, event KernelEvent) string {
	if event.BootID == "" {
		return fmt.Sprintf("kernel:%s:%d", id, event.EventID) // legacy snapshot
	}
	return fmt.Sprintf("kernel:%s:%s:%d", id, event.BootID, event.EventID)
}

// deliverKernel drains the durable kernel queues into the mailbox. Each
// event is persisted in the mailbox before it is acknowledged in the
// kernel, so a crash between the two steps replays through the idempotent
// key instead of losing or duplicating the delivery. A full mailbox keeps
// the kernel event unacked; the next drain retries it.
func (s *Supervisor) deliverKernel() error {
	if s.kernel == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	boot, err := s.kernel.BootID()
	if err != nil || boot == "" {
		return fmt.Errorf("kernel boot identity unavailable: %v", err)
	}
	ids := make([]string, 0, len(s.state.Applications))
	for id := range s.state.Applications {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		a := s.state.Applications[id]
		if a == nil || a.State == "tombstoned" || a.State == "retiring" ||
			a.KernelID == 0 {
			continue
		}
		s.markKernelRecoveryPendingLocked(id)
		recovered, err := s.deliverPersistedKernelLocked(id)
		if err != nil {
			s.markKernelRecoveryPendingLocked(id)
			return err
		}
		if !recovered {
			continue
		}
		h, err := s.ensureKernelHandleLocked(id)
		if err != nil {
			return err
		}
		for {
			event, err := h.Read()
			if err != nil {
				return err
			}
			if event == nil {
				break
			}
			identified := s.identifyKernelEventLocked(id, *event, boot)
			payload, merr := kernelWakePayload(id, *event)
			if merr == nil {
				_, merr = s.enqueueLocked("supervisor", id,
					kernelMessageKey(id, identified), payload)
			}
			if errors.Is(merr, ErrMailboxFull) {
				/* The cursor already moved past this event;
				 * drop the handle so the next drain reopens
				 * with a fresh cursor and replays every
				 * unacked event; idempotent enqueue absorbs
				 * the already-delivered prefix. */
				h.Close()
				delete(s.kernelApps, id)
				return nil // backpressure keeps events unacked
			}
			if merr != nil {
				s.restoreLocked()
				h.Close()
				delete(s.kernelApps, id)
				return merr
			}
			pending := s.state.KernelPending[id]
			kept := make([]KernelEvent, 0, len(pending)+1)
			for _, saved := range pending {
				if saved.BootID != identified.BootID || saved.EventID != identified.EventID {
					kept = append(kept, saved)
				}
			}
			s.state.KernelPending[id] = append(kept, identified)
			if err := s.persistLocked(nil); err != nil {
				s.restoreLocked()
				h.Close()
				delete(s.kernelApps, id)
				return err
			}
			if err := h.Ack(event.EventID); err != nil {
				h.Close()
				delete(s.kernelApps, id)
				return err
			}
			if len(kept) == 0 {
				delete(s.state.KernelPending, id)
			} else {
				s.state.KernelPending[id] = kept
			}
			if err := s.persistLocked(nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// snapshotKernel persists the unacked kernel queues for the next boot. It
// runs at orderly shutdown and opens handles on demand, so queues that
// were never drained this boot are still captured. An unclean VM reboot
// loses only the window between the last drain and the crash.
func (s *Supervisor) snapshotKernel() {
	if s.kernel == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	boot, err := s.kernel.BootID()
	if err != nil || boot == "" {
		return
	}
	ids := make([]string, 0, len(s.state.Applications))
	for id := range s.state.Applications {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	changed := false
	for _, id := range ids {
		a := s.state.Applications[id]
		if a == nil || a.State == "tombstoned" || a.State == "retiring" ||
			a.KernelID == 0 {
			continue
		}
		protected := false
		for _, saved := range s.state.KernelPending[id] {
			if saved.RecoveryPending {
				protected = true
			}
			if saved.QueueBootID != "" && saved.QueueBootID != boot {
				protected = true
			}
		}
		if protected {
			continue // recovery still owns the persisted snapshot
		}
		h, err := s.ensureKernelHandleLocked(id)
		if err != nil {
			continue // best effort; the same-boot queue keeps them unacked
		}
		events, err := h.Snapshot()
		if err != nil {
			continue
		}
		if len(events) == 0 {
			if _, ok := s.state.KernelPending[id]; ok {
				delete(s.state.KernelPending, id)
				changed = true
			}
			continue
		}
		for i := range events {
			events[i] = s.identifyKernelEventLocked(id, events[i], boot)
		}
		s.state.KernelPending[id] = events
		changed = true
	}
	s.closeKernelHandlesLocked()
	if changed {
		_ = s.persistLocked(nil)
	}
}
