package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
)

type blockingProvider struct{ entered chan struct{} }

func (p blockingProvider) Name() string { return "blocking" }
func (p blockingProvider) Complete(ctx context.Context, _ string) (string, Usage, error) {
	close(p.entered)
	<-ctx.Done()
	return "", Usage{InputTokens: 3}, ctx.Err()
}
func invoke(e *Engine, id, method, params string) Message {
	return e.Handle(Message{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: json.RawMessage(params)})
}
func TestAbortHasOneTerminalAndAccountsUsage(t *testing.T) {
	p := blockingProvider{entered: make(chan struct{})}
	e := NewEngineWithProvider(p)
	invoke(e, "1", "session/new", "{}")
	done := make(chan Message, 1)
	go func() { done <- invoke(e, "2", "session/prompt", `{"session_id":"session-1","request_id":"same"}`) }()
	<-p.entered
	if events := e.Events("session-1"); len(events) != 1 || events[0].Method != "session/started" {
		t.Fatalf("started not observable: %+v", events)
	}
	for i := 0; i < 2; i++ {
		invoke(e, "3", "session/abort", `{"session_id":"session-1","request_id":"same"}`)
	}
	result := <-done
	if string(result.Result) != `{"stop_reason":"cancelled"}` {
		t.Fatalf("result: %+v", result)
	}
	events := e.Events("session-1")
	if len(events) != 3 || events[1].Method != "session/usage" || events[2].Method != "session/done" {
		t.Fatalf("events: %+v", events)
	}
	for i, event := range events {
		var fields struct {
			SessionID string `json:"session_id"`
			TurnID    uint64 `json:"turn_id"`
			Seq       uint64 `json:"event_seq"`
		}
		if err := json.Unmarshal(event.Params, &fields); err != nil {
			t.Fatal(err)
		}
		if fields.SessionID != "session-1" || fields.TurnID != 1 || fields.Seq != uint64(i+1) {
			t.Fatalf("metadata: %+v", fields)
		}
	}
	if e.sessions["session-1"].ledger.Used != 3 {
		t.Fatal("cancelled usage lost")
	}
	replay := invoke(e, "99", "session/prompt", `{"session_id":"session-1","request_id":"same"}`)
	if string(replay.ID) != "99" || string(replay.Result) != string(result.Result) || len(e.Events("session-1")) != 3 {
		t.Fatalf("replay: %+v", replay)
	}
}
func TestSessionAccountingCloseAndEventIsolation(t *testing.T) {
	e := NewEngine()
	for _, id := range []string{"session-1", "session-2"} {
		invoke(e, "1", "session/new", "{}")
		invoke(e, "2", "session/prompt", `{"session_id":"`+id+`","text":"hi"}`)
		if e.sessions[id].ledger.Used != 4 {
			t.Fatalf("%s usage=%d", id, e.sessions[id].ledger.Used)
		}
	}
	events := e.Events("session-1")
	events[0].Params[0] = '!'
	if !json.Valid(e.Events("session-1")[0].Params) {
		t.Fatal("caller mutated event storage")
	}
	for i := 0; i < 2; i++ {
		if r := invoke(e, "3", "session/close", `{"session_id":"session-1"}`); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if r := invoke(e, "4", "session/prompt", `{"session_id":"session-1"}`); r.Error == nil {
		t.Fatal("closed session accepted prompt")
	}
	r := invoke(e, "5", "session/new", "{}")
	if string(r.Result) != `{"session_id":"session-3"}` {
		t.Fatalf("reused session id: %s", r.Result)
	}
}

type failureProvider struct{}

func (failureProvider) Name() string { return "failure" }
func (failureProvider) Complete(context.Context, string) (string, Usage, error) {
	return "", Usage{}, errors.New("private backend error")
}
func TestProviderFailureTerminal(t *testing.T) {
	e := NewEngineWithProvider(failureProvider{})
	invoke(e, "1", "session/new", "{}")
	r := invoke(e, "2", "session/prompt", `{"session_id":"session-1"}`)
	events := e.Events("session-1")
	if string(r.Result) != `{"stop_reason":"failed"}` || len(events) != 2 || events[1].Method != "session/failed" {
		t.Fatalf("failure: %+v %+v", r, events)
	}
}
func TestUsageArithmetic(t *testing.T) {
	if saturatingAdd(math.MaxUint64, 1) != math.MaxUint64 {
		t.Fatal("usage wrapped")
	}
	l := Ledger{HardLimit: 3, Used: 4}
	if err := l.Apply(1, 1, 1); err != ErrQuota {
		t.Fatalf("quota underflow: %v", err)
	}
}
