package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLlamaUsageAndCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/completion" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			Prompt string `json:"prompt"`
			Limit  int    `json:"n_predict"`
			Cache  bool   `json:"cache_prompt"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Prompt != "hello" || input.Limit != 32 || !input.Cache || input.Stream {
			t.Errorf("bad request: %+v %v", input, err)
		}
		w.Write([]byte(`{"content":"world","tokens_evaluated":12,"tokens_predicted":3,"tokens_cached":14,"timings":{"cache_n":8,"prompt_n":4}}`))
	}))
	defer server.Close()
	p, err := NewLlamaProvider(LlamaConfig{Endpoint: server.URL, MaxTokens: 32, CachePrompt: true})
	if err != nil {
		t.Fatal(err)
	}
	text, usage, err := p.Complete(context.Background(), "hello")
	if err != nil || text != "world" || usage != (Usage{InputTokens: 12, OutputTokens: 3, CachedTokens: 8}) {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestLlamaRejectsMalformedAccountingAndResponses(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		limit      int64
	}{
		{"missing usage", `{"content":"world"}`, 200, 0},
		{"negative usage", `{"content":"world","tokens_evaluated":-1,"tokens_predicted":1}`, 200, 0},
		{"fractional usage", `{"content":"world","tokens_evaluated":1.2,"tokens_predicted":1}`, 200, 0},
		{"overflow usage", `{"content":"world","tokens_evaluated":18446744073709551616,"tokens_predicted":1}`, 200, 0},
		{"excess output", `{"content":"world","tokens_evaluated":1,"tokens_predicted":129}`, 200, 0},
		{"excess cache", `{"content":"world","tokens_evaluated":1,"tokens_predicted":1,"timings":{"cache_n":2}}`, 200, 0},
		{"inconsistent cache", `{"content":"world","tokens_evaluated":3,"tokens_predicted":1,"timings":{"cache_n":1,"prompt_n":1}}`, 200, 0},
		{"truncated", `{"content":"world","tokens_evaluated":1,"tokens_predicted":1,"truncated":true}`, 200, 0},
		{"backend error", `{"error":{"message":"secret"}}`, 200, 0},
		{"status", `secret`, 503, 0},
		{"too large", strings.Repeat("x", 65), 200, 64},
		{"trailing data", `{"content":"world","tokens_evaluated":1,"tokens_predicted":1} {}`, 200, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer server.Close()
			p, err := NewLlamaProvider(LlamaConfig{Endpoint: server.URL, MaxResponseBytes: tc.limit})
			if err != nil {
				t.Fatal(err)
			}
			text, usage, err := p.Complete(context.Background(), "prompt")
			if err == nil || text != "" || usage != (Usage{}) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
			}
		})
	}
}

func TestLlamaCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	p, _ := NewLlamaProvider(LlamaConfig{Endpoint: server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := p.Complete(ctx, "hello"); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop HTTP request")
	}
}

func TestLlamaLimitsAndRedirect(t *testing.T) {
	for _, endpoint := range []string{"", "file:///tmp/model", "http://user:password@localhost", "http://localhost?secret=1", "http://localhost#secret"} {
		if _, err := NewLlamaProvider(LlamaConfig{Endpoint: endpoint}); err == nil {
			t.Errorf("accepted %q", endpoint)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/other", 302) }))
	defer server.Close()
	p, _ := NewLlamaProvider(LlamaConfig{Endpoint: server.URL})
	if _, _, err := p.Complete(context.Background(), "hello"); err == nil {
		t.Fatal("accepted redirect")
	}
	if _, _, err := p.Complete(context.Background(), strings.Repeat("x", maxTextBytes+1)); err == nil {
		t.Fatal("accepted oversized prompt")
	}
}

func TestLlamaCacheRequiresMeasuredUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":"text","tokens_evaluated":3,"tokens_predicted":1,"tokens_cached":3}`))
	}))
	defer server.Close()
	p, _ := NewLlamaProvider(LlamaConfig{Endpoint: server.URL, CachePrompt: true})
	if _, _, err := p.Complete(context.Background(), "hello"); err == nil {
		t.Fatal("accepted cache-enabled response without measured input cache usage")
	}
}

func TestLlamaDeadline(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	p, _ := NewLlamaProvider(LlamaConfig{Endpoint: server.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := p.Complete(ctx, "hello"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
}
