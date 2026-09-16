package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
)

func TestRoundTripAndLargeLine(t *testing.T) {
	var b bytes.Buffer
	in := Message{JSONRPC: "2.0", ID: json.RawMessage(`7`), Method: "session/prompt", Params: json.RawMessage(`{"text":"hi"}`)}
	if err := NewEncoder(&b).Write(in); err != nil {
		t.Fatal(err)
	}
	out := Message{}
	if err := NewDecoder(&b).Read(&out); err != nil {
		t.Fatal(err)
	}
	if out.Method != in.Method || string(out.ID) != "7" {
		t.Fatalf("round trip: %+v", out)
	}
	if err := NewDecoder(bytes.NewBufferString("{\"jsonrpc\":\"1.0\",\"method\":\"x\"}\n")).Read(&out); err == nil {
		t.Fatal("accepted wrong protocol")
	}
	if err := NewDecoder(bytes.NewBuffer(nil)).Read(&out); err != io.EOF {
		t.Fatal(err)
	}
}

func TestProviderCancellationAndUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := (EchoProvider{}).Complete(ctx, "x"); err == nil {
		t.Fatal("cancel ignored")
	}
	text, usage, err := (EchoProvider{}).Complete(context.Background(), "hello")
	if err != nil || text != "hello" || usage.InputTokens != 5 {
		t.Fatalf("provider: %q %+v %v", text, usage, err)
	}
}

func TestEngineLifecycle(t *testing.T) {
	e := NewEngine()
	call := func(id, method, params string) Message {
		return e.Handle(Message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: json.RawMessage(params)})
	}
	if call("1", "initialize", "{}").Error != nil {
		t.Fatal("initialize")
	}
	r := call("2", "session/new", "{}")
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(r.Result, &created); err != nil || created.SessionID == "" {
		t.Fatal("session/new")
	}
	if call("3", "session/prompt", `{"session_id":"`+created.SessionID+`","text":"hello","request_id":"r1"}`).Error != nil {
		t.Fatal("prompt")
	}
	first := len(e.Events(created.SessionID))
	if call("3b", "session/prompt", `{"session_id":"`+created.SessionID+`","text":"changed","request_id":"r1"}`).Error != nil || len(e.Events(created.SessionID)) != first {
		t.Fatal("prompt replay was not idempotent")
	}
	got := e.Events(created.SessionID)
	if len(got) != 4 || got[0].Method != "session/started" || got[3].Method != "session/done" {
		t.Fatalf("event order: %+v", got)
	}
	if call("4", "session/abort", `{"session_id":"`+created.SessionID+`"}`).Error != nil {
		t.Fatal("abort")
	}
	if call("5", "session/prompt", `{"session_id":"missing"}`).Error == nil {
		t.Fatal("unknown session accepted")
	}
}
