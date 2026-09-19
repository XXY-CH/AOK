//go:build linux && arm64

package kernelbridge

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Patch 0010 syscall and ioctl allocation.
const (
	nrEventSourceCreate = 484
	nrEventAck          = 486
	nrAppCreate         = 487
	nrAppSnapshot       = 488
	nrAppRestore        = 489

	ioEventAttachApp = 0xa022
	ioAppAck         = 0xa030

	SourceTimer uint32 = 1
	SourcePort  uint32 = 2
	SourceLSFS  uint32 = 3

	KindTimer uint32 = 1
	KindPort  uint32 = 2
	KindLSFS  uint32 = 3

	// AppQueueDepth mirrors AOK_APP_QUEUE_DEPTH; snapshot is bounded by it.
	AppQueueDepth = 128
)

// Layout guards mirror the UAPI structs so drift is a compile error.
type rawSourceAttr struct {
	Size                                     uint64
	ParentJobFd                              int32
	Kind, Flags, Reserved                    uint32
	FirstNs, IntervalNs                      uint64
	Reserved2                                [2]uint64
}

var _ [56-unsafe.Sizeof(rawSourceAttr{})]byte
var _ [unsafe.Sizeof(rawSourceAttr{}) - 56]byte

type rawAppAttr struct {
	Size                          uint64
	ParentJobFd                   int32
	Reserved                      uint32
	ApplicationID                 uint64
	Reserved2                     [2]uint64
}

var _ [40-unsafe.Sizeof(rawAppAttr{})]byte
var _ [unsafe.Sizeof(rawAppAttr{}) - 40]byte

type rawEventApp struct {
	ApplicationID uint64
	AppFd         int32
	Reserved      uint32
}

var _ [16-unsafe.Sizeof(rawEventApp{})]byte
var _ [unsafe.Sizeof(rawEventApp{}) - 16]byte

type rawEventRecord struct {
	Size, SourceKoid, ApplicationID, EventID, EventSeq uint64
	Kind, Coalesced                                    uint32
	Data                                               uint64
	Reserved                                           [2]uint64
}

var _ [72-unsafe.Sizeof(rawEventRecord{})]byte
var _ [unsafe.Sizeof(rawEventRecord{}) - 72]byte

type rawObjectInfo struct {
	Size, Aid  uint64
	Type, Stat uint32
	Rights     uint64
	Reserved   [3]uint64
}

// EventRecord is the durable-queue view of one kernel event.
type EventRecord struct {
	ApplicationID uint64
	EventID       uint64
	EventSeq      uint64
	Kind          uint32
	Coalesced     uint32
	Data          uint64
}

type EventSource struct{ file *os.File }
type Application struct{ file *os.File }

// Registry adapts the root capability into the supervisor event bridge.
type Registry struct{ root *Root }

// OpenRegistry claims the PID1 root capability for event wiring.
func OpenRegistry() (*Registry, error) {
	root, err := Bootstrap()
	if err != nil {
		return nil, err
	}
	return &Registry{root: root}, nil
}

func (r *Registry) Close() error { return r.root.Close() }

func fdOf(file *os.File) (uintptr, error) {
	var fd uintptr
	conn, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	if err := conn.Control(func(f uintptr) { fd = f }); err != nil {
		return 0, err
	}
	return fd, nil
}

// OpenApplication re-opens the registry entry for applicationID. Creation is
// idempotent per boot, which is the cross-process recovery path.
func (r *Registry) OpenApplication(applicationID uint64) (*Application, error) {
	parent, err := fdOf(r.root.file)
	if err != nil {
		return nil, err
	}
	attr := rawAppAttr{Size: 40, ParentJobFd: int32(parent), ApplicationID: applicationID}
	var info rawObjectInfo
	n, _, errno := syscall.Syscall(nrAppCreate, uintptr(unsafe.Pointer(&attr)),
		uintptr(unsafe.Pointer(&info)), 0)
	if errno != 0 {
		return nil, errno
	}
	if info.Type != 4 {
		if n >= 0 {
			syscall.Close(int(n))
		}
		return nil, fmt.Errorf("unexpected AOK object type %d", info.Type)
	}
	return &Application{file: os.NewFile(n, "aok-application")}, nil
}

// OpenEventSource creates a timer/port/LSFS producer under the root job.
func (r *Registry) OpenEventSource(kind uint32, firstNs, intervalNs uint64) (*EventSource, error) {
	parent, err := fdOf(r.root.file)
	if err != nil {
		return nil, err
	}
	attr := rawSourceAttr{Size: 56, ParentJobFd: int32(parent), Kind: kind,
		FirstNs: firstNs, IntervalNs: intervalNs}
	var info rawObjectInfo
	n, _, errno := syscall.Syscall(nrEventSourceCreate, uintptr(unsafe.Pointer(&attr)),
		uintptr(unsafe.Pointer(&info)), 0)
	if errno != 0 {
		return nil, errno
	}
	return &EventSource{file: os.NewFile(n, "aok-event-source")}, nil
}

func (e *EventSource) Close() error { return e.file.Close() }

// Attach routes every event this source fires into the application queue.
// The application_id is the registry key the caller opened the handle with.
func (e *EventSource) Attach(app *Application, applicationID uint64) error {
	appfd, err := fdOf(app.file)
	if err != nil {
		return err
	}
	attach := rawEventApp{ApplicationID: applicationID, AppFd: int32(appfd)}
	_, err = ioctl(e.file, ioEventAttachApp, unsafe.Pointer(&attach))
	return err
}

// ioctlValue passes an integer argument by value; the shared ioctl helper
// is only for by-reference structures.
func ioctlValue(file *os.File, command uintptr, value uint64) error {
	fd, err := fdOf(file)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, command,
		uintptr(value))
	if errno != 0 {
		return errno
	}
	return nil
}

// PostLSFS enqueues one LSFS commit notification carrying the cursor.
func (e *EventSource) PostLSFS(cursor uint64) error {
	return ioctlValue(e.file, 0xa021 /* AOK_EVENT_POST */, cursor)
}

// Ack retires an event through this source's attachment.
func (e *EventSource) Ack(eventID uint64) error {
	fd, err := fdOf(e.file)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(nrEventAck, fd, uintptr(eventID), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func (a *Application) Close() error { return a.file.Close() }

// Read returns the next unacked event from a private cursor, nil when empty.
func (a *Application) Read() (*EventRecord, error) {
	var raw rawEventRecord
	fd, err := fdOf(a.file)
	if err != nil {
		return nil, err
	}
	n, _, errno := syscall.Syscall(syscall.SYS_READ, fd,
		uintptr(unsafe.Pointer(&raw)), unsafe.Sizeof(raw))
	if errno != 0 {
		return nil, errno
	}
	if n == 0 {
		return nil, nil
	}
	return &EventRecord{ApplicationID: raw.ApplicationID, EventID: raw.EventID,
		EventSeq: raw.EventSeq, Kind: raw.Kind, Coalesced: raw.Coalesced,
		Data: raw.Data}, nil
}

// Ack retires one event from the durable queue.
func (a *Application) Ack(eventID uint64) error {
	return ioctlValue(a.file, ioAppAck, eventID)
}

// Snapshot copies the whole durable queue in sequence order.
func (a *Application) Snapshot() ([]EventRecord, error) {
	var raws [AppQueueDepth]rawEventRecord
	fd, err := fdOf(a.file)
	if err != nil {
		return nil, err
	}
	n, _, errno := syscall.Syscall(nrAppSnapshot, fd,
		uintptr(unsafe.Pointer(&raws[0])), AppQueueDepth)
	if errno != 0 {
		return nil, errno
	}
	if n < 0 || n > AppQueueDepth {
		return nil, fmt.Errorf("invalid AOK snapshot count %d", n)
	}
	out := make([]EventRecord, 0, n)
	for i := 0; i < int(n); i++ {
		out = append(out, EventRecord{ApplicationID: raws[i].ApplicationID,
			EventID: raws[i].EventID, EventSeq: raws[i].EventSeq,
			Kind: raws[i].Kind, Coalesced: raws[i].Coalesced,
			Data: raws[i].Data})
	}
	return out, nil
}

// Restore reinjects persisted records into the empty durable queue.
func (a *Application) Restore(events []EventRecord) error {
	if len(events) > AppQueueDepth {
		return fmt.Errorf("restore batch exceeds AOK queue depth")
	}
	if len(events) == 0 {
		return nil
	}
	raws := make([]rawEventRecord, len(events))
	for i, ev := range events {
		raws[i] = rawEventRecord{Size: 72, ApplicationID: ev.ApplicationID,
			EventID: ev.EventID, EventSeq: ev.EventSeq, Kind: ev.Kind,
			Coalesced: ev.Coalesced, Data: ev.Data}
	}
	fd, err := fdOf(a.file)
	if err != nil {
		return err
	}
	_, _, errno := syscall.Syscall(nrAppRestore, fd,
		uintptr(unsafe.Pointer(&raws[0])), uintptr(len(raws)))
	if errno != 0 {
		return errno
	}
	return nil
}
