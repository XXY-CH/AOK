package runtime

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startTestServer(t *testing.T, factory func() Provider) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	dir, err := os.MkdirTemp("", "aok-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "engine.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, factory) }()
	t.Cleanup(func() { cancel(); os.RemoveAll(dir) })
	return path, cancel, done
}

type wireClient struct {
	conn    net.Conn
	encoder *Encoder
	decoder *Decoder
}

func connectTest(t *testing.T, path string) *wireClient {
	t.Helper()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &wireClient{conn, NewEncoder(conn), NewDecoder(conn)}
}
func (c *wireClient) send(t *testing.T, id, method, params string) {
	t.Helper()
	if err := c.encoder.Write(Message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: json.RawMessage(params)}); err != nil {
		t.Fatal(err)
	}
}
func (c *wireClient) read(t *testing.T) Message {
	t.Helper()
	var message Message
	if err := c.decoder.Read(&message); err != nil {
		t.Fatal(err)
	}
	return message
}
func handshake(t *testing.T, c *wireClient, path string) {
	t.Helper()
	c.send(t, "1", "initialize", "{}")
	r := c.read(t)
	var result struct {
		Address string `json:"address"`
		Version int    `json:"aok_version"`
	}
	if err := json.Unmarshal(r.Result, &result); err != nil || result.Address != "unix://"+path || result.Version != 1 {
		t.Fatalf("handshake: %+v", r)
	}
}
func waitServer(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown stalled")
	}
}
func TestUnixTransportLifecycleAndIsolation(t *testing.T) {
	path, cancel, done := startTestServer(t, nil)
	c := connectTest(t, path)
	c.send(t, "0", "health", "{}")
	if r := c.read(t); r.Error == nil || r.Error.Code != -32002 {
		t.Fatalf("handshake bypass: %+v", r)
	}
	handshake(t, c, path)
	c.send(t, "2", "session/new", "{}")
	if r := c.read(t); string(r.Result) != `{"session_id":"session-1"}` {
		t.Fatalf("new: %+v", r)
	}
	for turn := 1; turn <= 2; turn++ {
		c.send(t, "3", "session/prompt", `{"session_id":"session-1","text":"hello"}`)
		for i, method := range []string{"session/started", "session/chunk", "session/usage", "session/done"} {
			event := c.read(t)
			var fields struct {
				Turn uint64 `json:"turn_id"`
				Seq  uint64 `json:"event_seq"`
			}
			if err := json.Unmarshal(event.Params, &fields); err != nil {
				t.Fatal(err)
			}
			if event.Method != method || fields.Turn != uint64(turn) || fields.Seq != uint64((turn-1)*4+i+1) {
				t.Fatalf("event: %+v", event)
			}
		}
		if r := c.read(t); string(r.ID) != "3" || string(r.Result) != `{"stop_reason":"completed"}` {
			t.Fatalf("prompt: %+v", r)
		}
	}
	other := connectTest(t, path)
	handshake(t, other, path)
	other.send(t, "2", "session/prompt", `{"session_id":"session-1"}`)
	if r := other.read(t); r.Error == nil {
		t.Fatal("cross-connection session accepted")
	}
	// Notifications have no response; the next frame must be the health response.
	c.send(t, "", "session/close", `{"session_id":"session-1"}`)
	c.send(t, "4", "health", "{}")
	if r := c.read(t); string(r.ID) != "4" {
		t.Fatalf("notification response: %+v", r)
	}
	c.send(t, "5", "session/prompt", `{"session_id":"session-1"}`)
	if r := c.read(t); r.Error == nil {
		t.Fatal("closed prompt accepted")
	}
	waitServer(t, cancel, done)
}
func TestUnixTransportAbortWhilePromptRunning(t *testing.T) {
	p := blockingProvider{entered: make(chan struct{})}
	path, cancel, done := startTestServer(t, func() Provider { return p })
	c := connectTest(t, path)
	handshake(t, c, path)
	c.send(t, "2", "session/new", "{}")
	c.read(t)
	// Pipeline abort immediately: the reader must establish the prompt before abort.
	c.send(t, "3", "session/prompt", `{"session_id":"session-1"}`)
	c.send(t, "4", "session/abort", `{"session_id":"session-1"}`)
	terminals, responses := 0, 0
	for i := 0; i < 5; i++ {
		r := c.read(t)
		if r.Method == "session/done" {
			terminals++
			if !strings.Contains(string(r.Params), `"stop_reason":"cancelled"`) {
				t.Fatalf("terminal: %+v", r)
			}
		}
		if len(r.ID) > 0 {
			responses++
			if string(r.Result) != `{"stop_reason":"cancelled"}` {
				t.Fatalf("response: %+v", r)
			}
		}
	}
	if terminals != 1 || responses != 2 {
		t.Fatalf("terminal=%d responses=%d", terminals, responses)
	}
	waitServer(t, cancel, done)
}
func TestUnixTransportMalformedFramesRecover(t *testing.T) {
	path, cancel, done := startTestServer(t, nil)
	c := connectTest(t, path)
	for _, tc := range []struct {
		frame string
		code  int
	}{
		{"{\n", -32700},
		{`{"jsonrpc":"2.0","id":{},"method":"initialize"}` + "\n", -32600},
		{`{"jsonrpc":"2.0","id":1,"method":"health","result":1}` + "\n", -32600},
	} {
		if _, err := c.conn.Write([]byte(tc.frame)); err != nil {
			t.Fatal(err)
		}
		r := c.read(t)
		if r.Error == nil || r.Error.Code != tc.code || string(r.ID) != "null" {
			t.Fatalf("invalid response: %+v", r)
		}
	}
	handshake(t, c, path)
	waitServer(t, cancel, done)
}

type disconnectProvider struct{ entered, cancelled chan struct{} }

func (p disconnectProvider) Name() string { return "disconnect" }
func (p disconnectProvider) Complete(ctx context.Context, _ string) (string, Usage, error) {
	close(p.entered)
	<-ctx.Done()
	close(p.cancelled)
	return "", Usage{}, ctx.Err()
}
func TestUnixDisconnectCancelsProvider(t *testing.T) {
	p := disconnectProvider{make(chan struct{}), make(chan struct{})}
	path, cancel, done := startTestServer(t, func() Provider { return p })
	c := connectTest(t, path)
	handshake(t, c, path)
	c.send(t, "2", "session/new", "{}")
	c.read(t)
	c.send(t, "3", "session/prompt", `{"session_id":"session-1"}`)
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not start")
	}
	c.conn.Close()
	select {
	case <-p.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not cancel provider")
	}
	waitServer(t, cancel, done)
}

func TestDecoderClearsReusedMessageAndReadsResponses(t *testing.T) {
	decoder := NewDecoder(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"health\"}\n{\"jsonrpc\":\"2.0\",\"method\":\"health\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n"))
	var m Message
	if err := decoder.Read(&m); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Read(&m); err != nil || len(m.ID) != 0 {
		t.Fatalf("stale ID: %+v %v", m, err)
	}
	if err := decoder.Read(&m); err != nil || m.Method != "" || string(m.ID) != "2" {
		t.Fatalf("response: %+v %v", m, err)
	}
}

func TestOutboundOverflowClosesSlowConnection(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); serveConnection(context.Background(), server, nil) }()
	encoder := NewEncoder(client)
	// Never read replies. The writer blocks and the bounded queue must overflow.
	for i := 0; i < outboundQueueSize+10; i++ {
		method := "health"
		if i == 0 {
			method = "initialize"
		}
		if err := encoder.Write(Message{JSONRPC: "2.0", ID: json.RawMessage("1"), Method: method}); err != nil {
			break
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("slow connection retained after queue overflow")
	}
}
