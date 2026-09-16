package runtime

import (
	"net/http/httptest"
	"testing"
)

func TestMetalProviderRequiresLoopback(t *testing.T) {
	if _, err := NewMetalProvider(MetalConfig{Endpoint: "https://example.com"}); err == nil {
		t.Fatal("accepted remote Metal endpoint")
	}
	server := httptest.NewServer(nil)
	defer server.Close()
	if _, err := NewMetalProvider(MetalConfig{Endpoint: server.URL}); err != nil {
		t.Fatal(err)
	}
}
