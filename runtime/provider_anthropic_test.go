package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicUsageAndHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost || r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("unexpected request %s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		var input struct {
			Model string `json:"model"`
			Max   int    `json:"max_tokens"`
			Msgs  []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Model != "test-model" || input.Max != 32 || len(input.Msgs) != 1 || input.Msgs[0].Content != "hello" {
			t.Fatalf("bad request: %+v %v", input, err)
		}
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"world"}],"usage":{"input_tokens":12,"output_tokens":3,"cache_read_input_tokens":4}}`))
	}))
	defer server.Close()
	p, err := NewAnthropicProvider(AnthropicConfig{APIKey: "test-key", Endpoint: server.URL + "/v1/messages", Model: "test-model", MaxTokens: 32})
	if err != nil {
		t.Fatal(err)
	}
	text, usage, err := p.Complete(context.Background(), "hello")
	if err != nil || text != "world" || usage != (Usage{InputTokens: 12, OutputTokens: 3, CachedTokens: 4}) {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestAnthropicRequiresKeyAndRejectsMalformedUsage(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	if _, err := NewAnthropicProvider(AnthropicConfig{}); err == nil {
		t.Fatal("accepted missing key")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"secret"}],"usage":{"input_tokens":1}}`))
	}))
	defer server.Close()
	p, err := NewAnthropicProvider(AnthropicConfig{APIKey: "test-key", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	text, usage, err := p.Complete(context.Background(), "hello")
	if err == nil || text != "" || usage != (Usage{}) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
}
