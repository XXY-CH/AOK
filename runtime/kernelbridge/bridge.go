// Package kernelbridge binds the experimental AOK arm64 kernel control ABI.
// It never substitutes a userspace implementation on unsupported kernels.
package kernelbridge

import (
	"errors"
	"os"
)

const (
	Infer uint64 = 1 << iota
	Export
	Delegate
	Revoke
	Serve
	All     = Infer | Export | Delegate | Revoke | Serve
	DataMax = 512
)

const (
	Idle uint32 = iota
	Pending
	Running
	Done
	Cancelled
)

var ErrUnsupported = errors.New("AOK kernel ABI requires Linux arm64 with CONFIG_AOK_EXPERIMENTAL")

type CapabilitySpec struct {
	Rights     uint64
	Taint      uint64
	ExportMask uint64
	TokenLimit uint64
}

type Record struct {
	Sequence     uint64
	InputTokens  uint64
	OutputTokens uint64
	TokensUsed   uint64
	Taint        uint64
	State        uint32
	Data         []byte
}

type Root struct{ file *os.File }
type Capability struct{ file *os.File }
type Client struct{ file *os.File }
type Backend struct{ file *os.File }

// File exposes the owned root descriptor for exec/SCM_RIGHTS handoff. The
// receiver keeps ownership; the caller must not close the returned file.
func (r *Root) File() *os.File { return r.file }

// RootFromFile wraps a root descriptor handed over by the guest PID1.
// Ownership transfers to the returned Root.
func RootFromFile(file *os.File) *Root { return &Root{file: file} }

func (r *Root) Close() error       { return r.file.Close() }
func (c *Capability) Close() error { return c.file.Close() }
func (c *Client) Close() error     { return c.file.Close() }
func (b *Backend) Close() error    { return b.file.Close() }

// File exposes the owned descriptor for exec/SCM_RIGHTS handoff. The receiver
// should receive only its designated endpoint; SERVE authority stays trusted.
func (c *Client) File() *os.File  { return c.file }
func (b *Backend) File() *os.File { return b.file }

func ClientFromFile(file *os.File) *Client   { return &Client{file: file} }
func BackendFromFile(file *os.File) *Backend { return &Backend{file: file} }
