//go:build linux

package runtime

import (
	"testing"
)

func TestVsockRejectsInvalidPort(t *testing.T) {
	if _, err := ListenVsock(unixAnyCID, 0); err == nil {
		t.Fatal("accepted zero listener port")
	}
	if _, err := DialVsock(unixAnyCID, 0); err == nil {
		t.Fatal("accepted zero dial port")
	}
}

const unixAnyCID = ^uint32(0)
