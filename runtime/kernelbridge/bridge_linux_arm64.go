//go:build linux && arm64

package kernelbridge

import (
	"errors"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	capCreate     = 0xa001
	capDerive     = 0xa002
	capRevoke     = 0xa003
	capCheck      = 0xa004
	sessionCreate = 0xa010
	submit        = 0xa011
	take          = 0xa012
	complete      = 0xa013
	result        = 0xa014
	cancel        = 0xa015
	export        = 0xa016
)

type rawIO struct {
	Sequence, InputTokens, OutputTokens, TokensUsed, Taint uint64
	State, Length                                          uint32
	Data                                                   [DataMax]byte
}

// Both directions make layout drift a compile-time error.
var _ [560 - unsafe.Sizeof(rawIO{})]byte
var _ [unsafe.Sizeof(rawIO{}) - 560]byte
var _ [32 - unsafe.Sizeof(CapabilitySpec{})]byte
var _ [unsafe.Sizeof(CapabilitySpec{}) - 32]byte

func ioctl(file *os.File, command uintptr, arg unsafe.Pointer) (int, error) {
	conn, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var value uintptr
	var errno syscall.Errno
	err = conn.Control(func(fd uintptr) {
		value, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, command, uintptr(arg))
	})
	runtime.KeepAlive(arg)
	if err != nil {
		return 0, err
	}
	if errno != 0 {
		return 0, errno
	}
	return int(value), nil
}

func Bootstrap() (*Root, error) {
	fd, _, errno := syscall.Syscall(473, 0, 0, 0)
	if errno != 0 {
		return nil, errno
	}
	return &Root{file: os.NewFile(fd, "aok-root")}, nil
}

func (r *Root) Create(spec CapabilitySpec) (*Capability, error) {
	fd, err := ioctl(r.file, capCreate, unsafe.Pointer(&spec))
	if err != nil {
		return nil, err
	}
	return &Capability{file: os.NewFile(uintptr(fd), "aok-cap")}, nil
}

func (c *Capability) Derive(spec CapabilitySpec) (*Capability, error) {
	fd, err := ioctl(c.file, capDerive, unsafe.Pointer(&spec))
	if err != nil {
		return nil, err
	}
	return &Capability{file: os.NewFile(uintptr(fd), "aok-cap")}, nil
}

func (c *Capability) Revoke() error { _, err := ioctl(c.file, capRevoke, nil); return err }
func (c *Capability) Check() error  { _, err := ioctl(c.file, capCheck, nil); return err }

func (c *Capability) Session() (*Client, *Backend, error) {
	var pair struct{ Client, Backend int32 }
	_, err := ioctl(c.file, sessionCreate, unsafe.Pointer(&pair))
	if err != nil {
		return nil, nil, err
	}
	return &Client{file: os.NewFile(uintptr(pair.Client), "aok-client")},
		&Backend{file: os.NewFile(uintptr(pair.Backend), "aok-backend")}, nil
}

func send(file *os.File, command uintptr, record Record) error {
	if len(record.Data) > DataMax {
		return errors.New("AOK payload exceeds 512 bytes")
	}
	if record.TokensUsed != 0 || record.State != 0 {
		return errors.New("AOK send record has output-only fields")
	}
	io := rawIO{Sequence: record.Sequence, InputTokens: record.InputTokens,
		OutputTokens: record.OutputTokens, Taint: record.Taint, Length: uint32(len(record.Data))}
	copy(io.Data[:], record.Data)
	_, err := ioctl(file, command, unsafe.Pointer(&io))
	return err
}

func receive(file *os.File, command uintptr) (Record, error) {
	var io rawIO
	_, err := ioctl(file, command, unsafe.Pointer(&io))
	if err != nil {
		return Record{}, err
	}
	if io.Length > DataMax {
		return Record{}, errors.New("invalid AOK payload length")
	}
	return Record{Sequence: io.Sequence, InputTokens: io.InputTokens, OutputTokens: io.OutputTokens,
		TokensUsed: io.TokensUsed, Taint: io.Taint, State: io.State,
		Data: append([]byte(nil), io.Data[:io.Length]...)}, nil
}

func (c *Client) Submit(record Record) error    { return send(c.file, submit, record) }
func (c *Client) Result() (Record, error)       { return receive(c.file, result) }
func (c *Client) Export() (Record, error)       { return receive(c.file, export) }
func (c *Client) Cancel() error                 { _, err := ioctl(c.file, cancel, nil); return err }
func (b *Backend) Take() (Record, error)        { return receive(b.file, take) }
func (b *Backend) Complete(record Record) error { return send(b.file, complete, record) }
