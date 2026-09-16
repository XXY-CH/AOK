//go:build linux

package runtime

import (
	"errors"
	"net"

	"github.com/mdlayher/vsock"
)

// ListenVsock binds an AF_VSOCK stream. CID 0xffffffff is the host wildcard;
// callers must choose the CID and port from trusted supervisor configuration.
func ListenVsock(cid, port uint32) (net.Listener, error) {
	if port == 0 {
		return nil, errors.New("vsock port must be non-zero")
	}
	return vsock.ListenContextID(cid, port, nil)
}

// DialVsock connects to a supervisor or engine AF_VSOCK stream.
func DialVsock(cid, port uint32) (net.Conn, error) {
	if port == 0 {
		return nil, errors.New("vsock port must be non-zero")
	}
	return vsock.Dial(cid, port, nil)
}
