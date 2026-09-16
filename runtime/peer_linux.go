package runtime

import (
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"os"
)

func controlPrincipal(conn net.Conn) (string, error) {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return "", fmt.Errorf("control requires unix socket")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return "", err
	}
	var cred *unix.Ucred
	var peerErr error
	err = raw.Control(func(fd uintptr) { cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err != nil {
		return "", err
	}
	if peerErr != nil {
		return "", peerErr
	}
	if int(cred.Uid) != os.Geteuid() {
		return "", fmt.Errorf("control peer denied")
	}
	return fmt.Sprintf("unix:uid:%d", cred.Uid), nil
}
