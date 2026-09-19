package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// KernelEvent is the durable-queue record of the kernel application registry.
type KernelEvent struct {
	ApplicationID uint64 `json:"application_id"`
	EventID       uint64 `json:"event_id"`
	EventSeq      uint64 `json:"event_seq"`
	Kind          uint32 `json:"kind"`
	Coalesced     uint32 `json:"coalesced"`
	Data          uint64 `json:"data"`
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

// attachKernel restores persisted kernel pending queues into the registry.
// After a VM reboot the kernel queue is empty and the restore rebuilds it;
// after a same-boot supervisor restart the live kernel queue already holds
// the events, so the persisted copy is dropped and idempotent enqueue
// deduplicates the re-drain.
func (s *Supervisor) attachKernel() error {
	if s.kernel == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.state.Applications))
	for id := range s.state.Applications {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	restored := false
	for _, id := range ids {
		a := s.state.Applications[id]
		pending := s.state.KernelPending[id]
		if a == nil || a.State == "tombstoned" || len(pending) == 0 {
			continue
		}
		h, err := s.ensureKernelHandleLocked(id)
		if err != nil {
			return err
		}
		if err := h.Restore(pending); err != nil {
			events, serr := h.Snapshot()
			if serr != nil || len(events) == 0 {
				if serr == nil {
					serr = err
				}
				h.Close()
				delete(s.kernelApps, id)
				return serr
			}
			// The live queue from this boot stays authoritative.
		}
		delete(s.state.KernelPending, id)
		_ = s.auditLocked("supervisor", id, "kernel.restore",
			fmt.Sprintf("events=%d", len(pending)), "allow", "")
		restored = true
	}
	if restored {
		return s.persistLocked(nil)
	}
	return nil
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
			payload, merr := json.Marshal(KernelWakeEvent{
				SourceKind: "kernel", ApplicationID: id,
				EventID: event.EventID, EventSeq: event.EventSeq,
				KernelKind: kernelKindName(event.Kind),
				Coalesced:  event.Coalesced, Data: event.Data,
				Text: fmt.Sprintf("kernel %s event: event_id=%d seq=%d data=%d",
					kernelKindName(event.Kind), event.EventID,
					event.EventSeq, event.Data),
			})
			if merr == nil {
				_, merr = s.enqueueLocked("supervisor", id,
					fmt.Sprintf("kernel:%s:%d", id, event.EventID), payload)
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
				return merr
			}
			if err := s.persistLocked(nil); err != nil {
				s.restoreLocked()
				return err
			}
			if err := h.Ack(event.EventID); err != nil {
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
		s.state.KernelPending[id] = events
		changed = true
	}
	s.closeKernelHandlesLocked()
	if changed {
		_ = s.persistLocked(nil)
	}
}
