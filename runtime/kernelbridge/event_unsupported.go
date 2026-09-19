//go:build !linux || !arm64

package kernelbridge

const (
	SourceTimer uint32 = 1
	SourcePort  uint32 = 2
	SourceLSFS  uint32 = 3

	KindTimer uint32 = 1
	KindPort  uint32 = 2
	KindLSFS  uint32 = 3
)

type EventRecord struct {
	ApplicationID uint64
	EventID       uint64
	EventSeq      uint64
	Kind          uint32
	Coalesced     uint32
	Data          uint64
}

type EventSource struct{}
type Application struct{}
type Registry struct{}

func OpenRegistry() (*Registry, error) { return nil, ErrUnsupported }

func (r *Registry) Close() error { return ErrUnsupported }

func (r *Registry) OpenApplication(uint64) (*Application, error) {
	return nil, ErrUnsupported
}

func (r *Registry) OpenEventSource(uint32, uint64, uint64) (*EventSource, error) {
	return nil, ErrUnsupported
}

func (e *EventSource) Close() error                                  { return ErrUnsupported }
func (e *EventSource) Attach(*Application, uint64) error             { return ErrUnsupported }
func (e *EventSource) PostLSFS(uint64) error                         { return ErrUnsupported }
func (e *EventSource) Ack(uint64) error                              { return ErrUnsupported }
func (a *Application) Close() error                                  { return ErrUnsupported }
func (a *Application) Read() (*EventRecord, error)                   { return nil, ErrUnsupported }
func (a *Application) Ack(uint64) error                              { return ErrUnsupported }
func (a *Application) Snapshot() ([]EventRecord, error)              { return nil, ErrUnsupported }
func (a *Application) Restore([]EventRecord) error                   { return ErrUnsupported }
