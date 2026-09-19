//go:build linux && arm64

package runtime

import (
	"os"
	"strconv"

	"aok/runtime/kernelbridge"
)

type kernelBridgeAdapter struct {
	registry *kernelbridge.Registry
}

type kernelAppAdapter struct {
	app *kernelbridge.Application
}

type kernelSourceAdapter struct {
	source *kernelbridge.EventSource
}

// OpenKernelEventBridge connects the kernel application registry. It first
// honors a root descriptor handed over by the guest PID1 via AOK_ROOT_FD;
// otherwise it claims the root capability itself, which only PID1 may do.
// Any failure leaves the supervisor on the pure user-space path.
func OpenKernelEventBridge() (KernelEventBridge, error) {
	if fd := os.Getenv("AOK_ROOT_FD"); fd != "" {
		number, err := strconv.Atoi(fd)
		if err != nil || number < 0 {
			return nil, kernelbridge.ErrUnsupported
		}
		file := os.NewFile(uintptr(number), "aok-root")
		if file == nil {
			return nil, kernelbridge.ErrUnsupported
		}
		return KernelEventBridgeFromRoot(kernelbridge.RootFromFile(file))
	}
	registry, err := kernelbridge.OpenRegistry()
	if err != nil {
		return nil, err
	}
	return kernelBridgeAdapter{registry: registry}, nil
}

// KernelEventBridgeFromRoot adapts an already-claimed root capability; the
// init process owns the only claim in the boot.
func KernelEventBridgeFromRoot(root *kernelbridge.Root) (KernelEventBridge, error) {
	if root == nil {
		return nil, kernelbridge.ErrUnsupported
	}
	registry := kernelbridge.RegistryFromRoot(root)
	return kernelBridgeAdapter{registry: registry}, nil
}

func (b kernelBridgeAdapter) Open(applicationID uint64) (KernelApplication, error) {
	app, err := b.registry.OpenApplication(applicationID)
	if err != nil {
		return nil, err
	}
	return kernelAppAdapter{app: app}, nil
}

func (b kernelBridgeAdapter) OpenSource(kind uint32) (KernelEventSource, error) {
	source, err := b.registry.OpenEventSource(kind, 0, 0)
	if err != nil {
		return nil, err
	}
	return kernelSourceAdapter{source: source}, nil
}

func (s kernelSourceAdapter) Attach(app KernelApplication, applicationID uint64) error {
	target, ok := app.(kernelAppAdapter)
	if !ok {
		return kernelbridge.ErrUnsupported
	}
	return s.source.Attach(target.app, applicationID)
}

func (s kernelSourceAdapter) Post(cursor uint64) error {
	return s.source.PostLSFS(cursor)
}

func (s kernelSourceAdapter) Close() error { return s.source.Close() }

func (a kernelAppAdapter) Read() (*KernelEvent, error) {
	event, err := a.app.Read()
	if err != nil || event == nil {
		return nil, err
	}
	return &KernelEvent{ApplicationID: event.ApplicationID,
		EventID: event.EventID, EventSeq: event.EventSeq,
		Kind: event.Kind, Coalesced: event.Coalesced,
		Data: event.Data}, nil
}

func (a kernelAppAdapter) Ack(eventID uint64) error  { return a.app.Ack(eventID) }
func (a kernelAppAdapter) Close() error              { return a.app.Close() }
func (a kernelAppAdapter) Snapshot() ([]KernelEvent, error) {
	events, err := a.app.Snapshot()
	if err != nil {
		return nil, err
	}
	out := make([]KernelEvent, 0, len(events))
	for _, event := range events {
		out = append(out, KernelEvent{ApplicationID: event.ApplicationID,
			EventID: event.EventID, EventSeq: event.EventSeq,
			Kind: event.Kind, Coalesced: event.Coalesced,
			Data: event.Data})
	}
	return out, nil
}

func (a kernelAppAdapter) Restore(events []KernelEvent) error {
	records := make([]kernelbridge.EventRecord, len(events))
	for i, event := range events {
		records[i] = kernelbridge.EventRecord{
			ApplicationID: event.ApplicationID, EventID: event.EventID,
			EventSeq: event.EventSeq, Kind: event.Kind,
			Coalesced: event.Coalesced, Data: event.Data}
	}
	return a.app.Restore(records)
}
