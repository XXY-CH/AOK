//go:build !linux

package runtime

import (
	"errors"
	"net"
)

var ErrVsockUnsupported = errors.New("AF_VSOCK is unsupported on this host")

func ListenVsock(uint32, uint32) (net.Listener, error) { return nil, ErrVsockUnsupported }
func DialVsock(uint32, uint32) (net.Conn, error)       { return nil, ErrVsockUnsupported }
