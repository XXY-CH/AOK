//go:build linux && arm64

package runtime

import (
	"aok/runtime/kernelbridge"
)

type kernelBridgeAdapter struct {
	registry *kernelbridge.Registry
}

type kernelAppAdapter struct {
	app *kernelbridge.Application
}

// OpenKernelEventBridge claims the PID1 root capability and adapts the
// kernel application registry into the supervisor event bridge. It fails
// on stock kernels; the supervisor then stays purely user-space.
func OpenKernelEventBridge() (KernelEventBridge, error) {
	registry, err := kernelbridge.OpenRegistry()
	if err != nil {
		return nil, err
	}
	return kernelBridgeAdapter{registry: registry}, nil
}

func (b kernelBridgeAdapter) Open(applicationID uint64) (KernelApplication, error) {
	app, err := b.registry.OpenApplication(applicationID)
	if err != nil {
		return nil, err
	}
	return kernelAppAdapter{app: app}, nil
}

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
