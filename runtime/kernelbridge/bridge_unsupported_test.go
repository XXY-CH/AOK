//go:build !linux || !arm64

package kernelbridge

import (
	"errors"
	"testing"
)

func TestUnsupportedFailsClosed(t *testing.T) {
	if root, err := Bootstrap(); root != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("bootstrap = %v, %v", root, err)
	}
	if err := (&Client{}).Submit(Record{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("submit = %v", err)
	}
	if _, err := (&Backend{}).Take(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("take = %v", err)
	}
}
