package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

type countingProvider struct {
	calls int
	text  string
}

func (p *countingProvider) Name() string { return "counting" }
func (p *countingProvider) Complete(context.Context, string) (string, Usage, error) {
	p.calls++
	return p.text, Usage{OutputTokens: 1}, nil
}
func promptParams(session, key, text string) string {
	b, _ := json.Marshal(map[string]string{"session_id": session, "request_id": key, "text": text})
	return string(b)
}
func TestSessionCapacityAndCloseReclamation(t *testing.T) {
	e := NewEngine()
	for i := 0; i < maxSessions; i++ {
		if r := invoke(e, "1", "session/new", "{}"); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if r := invoke(e, "1", "session/new", "{}"); r.Error == nil || r.Error.Code != -32003 {
		t.Fatalf("capacity: %+v", r)
	}
	for i := 0; i < maxSessions*2; i++ {
		session := fmt.Sprintf("session-%d", i+1)
		invoke(e, "2", "session/prompt", promptParams(session, "r", "hi"))
		for n := 0; n < 2; n++ {
			if r := invoke(e, "3", "session/close", promptParams(session, "", "")); r.Error != nil {
				t.Fatal(r.Error)
			}
		}
		if _, ok := e.sessions[session]; ok || len(e.Events(session)) != 0 {
			t.Fatal("closed state retained")
		}
		if r := invoke(e, "4", "session/prompt", promptParams(session, "r", "hi")); r.Error == nil {
			t.Fatal("closed side effect replayed")
		}
		if r := invoke(e, "5", "session/new", "{}"); r.Error != nil {
			t.Fatal(r.Error)
		}
		if len(e.sessions) != maxSessions {
			t.Fatal("session slot was not recycled")
		}
	}
	for _, id := range []string{"session-0", "session-01", "session-+1", "session-9999"} {
		if r := invoke(e, "6", "session/close", promptParams(id, "", "")); r.Error == nil {
			t.Fatalf("unissued ID accepted: %s", id)
		}
	}
	e.Close()
	if len(e.sessions) != 0 || len(e.events) != 0 {
		t.Fatal("engine close retained history")
	}
}
func TestReplayCapacityNeverRepeatsAcceptedWork(t *testing.T) {
	p := &countingProvider{text: "hello"}
	e := NewEngineWithProvider(p)
	invoke(e, "1", "session/new", "{}")
	for i := 0; i < maxReplayRequests; i++ {
		r := invoke(e, "2", "session/prompt", promptParams("session-1", fmt.Sprint(i), "hi"))
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		// Caller mutation must not poison a cached result.
		r.Result[0] = '!'
	}
	if r := invoke(e, "3", "session/prompt", promptParams("session-1", "new", "hi")); r.Error == nil || r.Error.Code != -32003 {
		t.Fatalf("capacity: %+v", r)
	}
	r := invoke(e, "99", "session/prompt", promptParams("session-1", "0", "changed"))
	if r.Error != nil || string(r.ID) != "99" || string(r.Result) != `{"stop_reason":"completed"}` || p.calls != maxReplayRequests {
		t.Fatalf("replay caused work/corruption: %+v calls=%d", r, p.calls)
	}
	for i := 0; i < maxReplayRequests*2; i++ {
		if r := invoke(e, "4", "session/abort", promptParams("session-1", fmt.Sprint(i), "")); r.Error != nil {
			t.Fatal(r.Error)
		}
	}
	if len(e.sessions["session-1"].requests) != maxReplayRequests {
		t.Fatal("control requests grew cache")
	}
	if r := invoke(e, "5", "session/close", promptParams("session-1", "close", "")); r.Error != nil || len(e.sessions) != 0 {
		t.Fatal("full cache blocked close")
	}
}
func TestHistoryCountBytesAndExpiredCursor(t *testing.T) {
	for _, text := range []string{"small", strings.Repeat("\x00", maxTextBytes)} {
		e := NewEngine()
		invoke(e, "1", "session/new", "{}")
		turns := maxHistoryEvents/4 + 2
		if len(text) > 5 {
			turns = 4
		}
		for i := 0; i < turns; i++ {
			invoke(e, "2", "session/prompt", promptParams("session-1", "", text))
		}
		s := e.sessions["session-1"]
		if len(e.Events("session-1")) > maxHistoryEvents || s.historyBytes > maxHistoryBytes || s.historyFloor == 0 {
			t.Fatal("history not bounded")
		}
		if _, err := e.EventsSince("session-1", 0); err != ErrEventHistoryExpired {
			t.Fatalf("silent gap: %v", err)
		}
		events, err := e.EventsSince("session-1", s.historyFloor)
		if err != nil || len(events) == 0 {
			t.Fatalf("retained cursor: %v", err)
		}
		var first struct {
			Seq uint64 `json:"event_seq"`
		}
		json.Unmarshal(events[0].Params, &first)
		if first.Seq != s.historyFloor+1 {
			t.Fatal("incorrect history floor")
		}
	}
}
func TestRequestAndProviderOutputLimits(t *testing.T) {
	p := &countingProvider{text: strings.Repeat("x", maxTextBytes+1)}
	e := NewEngineWithProvider(p)
	invoke(e, "1", "session/new", "{}")
	for _, params := range []string{promptParams("session-1", strings.Repeat("x", maxRequestIDBytes+1), ""), promptParams("session-1", "", strings.Repeat("x", maxTextBytes+1))} {
		if r := invoke(e, "2", "session/prompt", params); r.Error == nil || p.calls != 0 {
			t.Fatal("oversized input reached provider")
		}
	}
	r := invoke(e, "3", "session/prompt", promptParams("session-1", "r", "hi"))
	if string(r.Result) != `{"stop_reason":"failed"}` || e.sessions["session-1"].ledger.Used != 1 {
		t.Fatalf("output failure/accounting: %+v", r)
	}
	events := e.Events("session-1")
	if len(events) != 3 || events[1].Method != "session/usage" || events[2].Method != "session/failed" {
		t.Fatalf("oversized output delivered: %+v", events)
	}
}
func TestCloseWaitsForProviderBeforeReleasingSlot(t *testing.T) {
	p := disconnectProvider{make(chan struct{}), make(chan struct{})}
	e := NewEngineWithProvider(p)
	invoke(e, "1", "session/new", "{}")
	done := make(chan Message, 1)
	go func() { done <- invoke(e, "2", "session/prompt", promptParams("session-1", "r", "")) }()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not start")
	}
	if r := invoke(e, "3", "session/close", promptParams("session-1", "", "")); r.Error != nil {
		t.Fatal(r.Error)
	}
	select {
	case r := <-done:
		if string(r.Result) != `{"stop_reason":"cancelled"}` {
			t.Fatal("not cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close stalled")
	}
	if len(e.sessions) != 0 || len(e.events) != 0 {
		t.Fatal("active close retained state")
	}
}
func TestOutboundByteLimitClosesSlowConnection(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan struct{})
	go func() { defer close(done); serveConnection(context.Background(), server, nil) }()
	enc := NewEncoder(client)
	// Fewer than 256 messages, but more than 8 MiB of response IDs. No reads.
	largeID := json.RawMessage(`"` + strings.Repeat("x", 2<<20) + `"`)
	for i := 0; i < 6; i++ {
		method := "health"
		if i == 0 {
			method = "initialize"
		}
		if err := enc.Write(Message{JSONRPC: "2.0", ID: largeID, Method: method}); err != nil {
			break
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("byte budget failed to terminate slow peer")
	}
}
