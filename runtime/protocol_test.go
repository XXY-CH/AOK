package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
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

func TestEngineSessionSuspendResumeReplaysRetainedEvents(t *testing.T) {
	e := NewEngine()
	var delivered []Message
	e.onEvent = func(m Message) bool { delivered = append(delivered, m); return true }
	call := func(id, method, params string) Message {
		return e.Handle(Message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: json.RawMessage(params)})
	}
	if r := call("1", "session/new", "{}"); r.Error != nil {
		t.Fatal(r.Error)
	}
	if r := call("2", "session/suspend", `{"session_id":"session-1"}`); r.Error != nil || string(r.Result) != `{"status":"suspended"}` {
		t.Fatalf("suspend: %+v", r)
	}
	if r := call("3", "session/prompt", promptParams("session-1", "suspended", "hello")); r.Error == nil || r.Error.Code != -32004 {
		t.Fatalf("suspended prompt: %+v", r)
	}
	if r := call("4", "session/resume", `{"session_id":"session-1"}`); r.Error != nil || string(r.Result) != `{"status":"resumed"}` {
		t.Fatalf("resume without events: %+v", r)
	}
	p := &suspendProvider{entered: make(chan struct{}), release: make(chan struct{})}
	e.provider = p
	promptDone := make(chan Message, 1)
	go func() { promptDone <- call("5", "session/prompt", promptParams("session-1", "deferred", "hello")) }()
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	if r := call("6", "session/suspend", `{"session_id":"session-1"}`); r.Error != nil {
		t.Fatal(r.Error)
	}
	close(p.release)
	if r := <-promptDone; r.Error != nil {
		t.Fatalf("deferred prompt: %+v", r)
	}
	if len(delivered) != 1 || delivered[0].Method != "session/started" {
		t.Fatalf("suspended session delivery: %+v", delivered)
	}
	if r := call("7", "session/resume", `{"session_id":"session-1"}`); r.Error != nil {
		t.Fatalf("resume: %+v", r)
	}
	if len(delivered) != 4 || delivered[0].Method != "session/started" || delivered[3].Method != "session/done" {
		t.Fatalf("replayed events: %+v", delivered)
	}
	for i, event := range delivered {
		if eventSequence(event) != uint64(i+1) {
			t.Fatalf("event sequence: %+v", delivered)
		}
	}
	if r := call("8", "session/prompt", promptParams("session-1", "after", "again")); r.Error != nil {
		t.Fatalf("post-resume prompt: %+v", r)
	}
	if len(delivered) != 8 || eventSequence(delivered[7]) != 8 {
		t.Fatalf("post-resume delivery: %+v", delivered)
	}
}

// Transport queue overflow must suspend delivery rather than drop the connection:
// the events stay retained and session/resume replays them in order.
func TestRefusedEventDeliverySuspendsSessionForResume(t *testing.T) {
	e := NewEngine()
	var delivered []Message
	accept := true
	e.onEvent = func(m Message) bool {
		if !accept {
			return false
		}
		delivered = append(delivered, m)
		return true
	}
	call := func(id, method, params string) Message {
		return e.Handle(Message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: json.RawMessage(params)})
	}
	if r := call("1", "session/new", "{}"); r.Error != nil {
		t.Fatal(r.Error)
	}
	accept = false
	if r := call("2", "session/prompt", promptParams("session-1", "backpressured", "hello")); r.Error != nil {
		t.Fatalf("overflow failed the turn: %+v", r)
	}
	if len(delivered) != 0 {
		t.Fatalf("delivered while refused: %+v", delivered)
	}
	if !e.sessions["session-1"].suspended {
		t.Fatal("refused delivery did not suspend the session")
	}
	accept = true
	if r := call("3", "session/resume", `{"session_id":"session-1"}`); r.Error != nil {
		t.Fatalf("resume: %+v", r)
	}
	if len(delivered) != 4 || delivered[0].Method != "session/started" || delivered[3].Method != "session/done" {
		t.Fatalf("replay after resume: %+v", delivered)
	}
	for i, event := range delivered {
		if eventSequence(event) != uint64(i+1) {
			t.Fatalf("replay order: %+v", delivered)
		}
	}
}

type suspendProvider struct {
	entered, release chan struct{}
	once             sync.Once
}

func (p *suspendProvider) Name() string { return "suspend-test" }
func (p *suspendProvider) Complete(context.Context, string) (string, Usage, error) {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return "hello", Usage{InputTokens: 1, OutputTokens: 1}, nil
}
