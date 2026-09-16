package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"
)

type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Engine struct {
	mu          sync.Mutex
	sessions    map[string]*engineSession
	events      map[string][]Message
	provider    Provider
	nextSession uint64
	onEvent     func(Message)
	closed      bool
}
type engineSession struct {
	cancel          context.CancelFunc
	running, closed bool
	turn, eventSeq  uint64
	requests        map[string]Message
	ledger          Ledger
	historyBytes    int
	historyFloor    uint64
}

func NewEngine() *Engine { return NewEngineWithProvider(EchoProvider{}) }
func NewEngineWithProvider(provider Provider) *Engine {
	if provider == nil {
		provider = EchoProvider{}
	}
	return &Engine{sessions: make(map[string]*engineSession), events: make(map[string][]Message), provider: provider}
}

// emit is called with e.mu held. Sequence numbers are scoped to a session.
func (e *Engine) emit(id string, s *engineSession, method string, fields map[string]any) {
	s.eventSeq++
	fields["session_id"], fields["turn_id"], fields["event_seq"] = id, s.turn, s.eventSeq
	params, _ := json.Marshal(fields)
	event := Message{JSONRPC: "2.0", Method: method, Params: params}
	if e.onEvent != nil {
		e.onEvent(event)
		s.historyFloor = s.eventSeq
	} else {
		history := e.events[id]
		s.historyBytes += len(params)
		history = append(history, event)
		for len(history) > maxHistoryEvents || s.historyBytes > maxHistoryBytes {
			s.historyBytes -= len(history[0].Params)
			s.historyFloor++
			history[0] = Message{}
			history = history[1:]
		}
		e.events[id] = history
	}
}
func (e *Engine) Handle(m Message) Message {
	return e.handle(m, func() {})
}

// ready marks the point at which a prompt is cancellable. The transport waits
// for this point before dispatching a following abort from the same connection.
func (e *Engine) handle(m Message, ready func()) Message {
	defer ready()
	resp := Message{JSONRPC: "2.0", ID: m.ID}
	switch m.Method {
	case "initialize":
		resp.Result, _ = json.Marshal(map[string]any{"aok_version": 1, "engine": e.provider.Name(), "capabilities": []string{"cancel", "usage"}, "limits": map[string]int{
			"sessions": maxSessions, "replay_requests_per_session": maxReplayRequests,
			"request_id_bytes": maxRequestIDBytes, "text_bytes": maxTextBytes,
		}})
		return resp
	case "health":
		resp.Result = json.RawMessage(`{"status":"ok"}`)
		return resp
	case "session/new":
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return Message{JSONRPC: "2.0", ID: m.ID, Error: &RPCError{Code: -32000, Message: "engine closed"}}
		}
		if len(e.sessions) >= maxSessions || e.nextSession == math.MaxUint64 {
			e.mu.Unlock()
			resp.Error = &RPCError{Code: -32003, Message: "session capacity reached"}
			return resp
		}
		e.nextSession++
		id := fmt.Sprintf("session-%d", e.nextSession)
		e.sessions[id] = &engineSession{requests: make(map[string]Message)}
		e.mu.Unlock()
		resp.Result, _ = json.Marshal(map[string]string{"session_id": id})
		return resp
	case "session/prompt", "session/abort", "session/close":
	default:
		resp.Error = &RPCError{Code: -32601, Message: "method not found"}
		return resp
	}
	var p struct {
		SessionID string `json:"session_id"`
		Text      string `json:"text"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(m.Params, &p) != nil || p.SessionID == "" {
		resp.Error = &RPCError{Code: -32602, Message: "invalid params"}
		return resp
	}
	if len(p.RequestID) > maxRequestIDBytes || len(p.Text) > maxTextBytes {
		resp.Error = &RPCError{Code: -32602, Message: "request exceeds engine limits"}
		return resp
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[p.SessionID]
	if !ok {
		if e.issuedSession(p.SessionID) {
			if m.Method == "session/close" || m.Method == "session/abort" {
				resp.Result = json.RawMessage(`{"stop_reason":"cancelled"}`)
			} else {
				resp.Error = &RPCError{Code: -32602, Message: "session closed"}
			}
		} else {
			resp.Error = &RPCError{Code: -32602, Message: "unknown session"}
		}
		return resp
	}
	key := m.Method + ":" + p.RequestID
	if m.Method == "session/prompt" && p.RequestID != "" {
		if prior, found := s.requests[key]; found {
			prior.ID = m.ID
			prior.Result = append(json.RawMessage(nil), prior.Result...)
			return prior
		}
	}
	if m.Method == "session/abort" || m.Method == "session/close" {
		if s.running {
			s.cancel()
		}
		if m.Method == "session/close" {
			s.closed = true
		}
		resp.Result = json.RawMessage(`{"stop_reason":"cancelled"}`)
		if s.closed && !s.running {
			e.releaseSession(p.SessionID)
		}
		return resp
	}
	if s.closed {
		resp.Error = &RPCError{Code: -32602, Message: "session closed"}
		return resp
	}
	if s.running {
		resp.Error = &RPCError{Code: -32001, Message: "turn already running"}
		return resp
	}
	if p.RequestID != "" && len(s.requests) >= maxReplayRequests {
		resp.Error = &RPCError{Code: -32003, Message: "replay capacity reached; close session before starting a new one"}
		return resp
	}
	if s.turn == math.MaxUint64 || s.eventSeq > math.MaxUint64-4 {
		resp.Error = &RPCError{Code: -32003, Message: "session sequence exhausted"}
		return resp
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.cancel, s.running = cancel, true
	s.turn++
	e.emit(p.SessionID, s, "session/started", map[string]any{})
	e.mu.Unlock()
	ready()
	text, usage, err := e.provider.Complete(ctx, p.Text)
	e.mu.Lock()
	// Keep the turn running until accounting, events and replay result are committed.
	reason := "completed"
	if err != nil || len(text) > maxTextBytes {
		reason = "failed"
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		reason = "cancelled"
	}
	// Providers may report billable usage even on cancellation or failure.
	if usage != (Usage{}) {
		if accountErr := s.ledger.Apply(s.turn, s.turn, saturatingAdd(usage.InputTokens, usage.OutputTokens)); accountErr != nil {
			reason = "budget_exhausted"
		}
	}
	if reason == "completed" {
		e.emit(p.SessionID, s, "session/chunk", map[string]any{"text": text})
	}
	if usage != (Usage{}) {
		e.emit(p.SessionID, s, "session/usage", map[string]any{"usage": usage})
	}
	terminal := "session/done"
	if reason == "failed" {
		terminal = "session/failed"
	}
	e.emit(p.SessionID, s, terminal, map[string]any{"stop_reason": reason})
	resp.Result, _ = json.Marshal(map[string]string{"stop_reason": reason})
	if p.RequestID != "" {
		remember(s, key, resp)
	}
	s.running = false
	s.cancel = nil
	if s.closed {
		e.releaseSession(p.SessionID)
	}
	return resp
}

// Events returns only the retained diagnostic tail. Use EventsSince to detect gaps.
func (e *Engine) Events(sessionID string) []Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := append([]Message(nil), e.events[sessionID]...)
	for i := range result {
		result[i].Params = append(json.RawMessage(nil), result[i].Params...)
	}
	return result
}

// Close cancels connection-scoped work. It does not retire an AOK Application.
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for id, s := range e.sessions {
		s.closed = true
		if s.running {
			s.cancel()
		} else {
			e.releaseSession(id)
		}
	}
}

type Encoder struct{ w io.Writer }

func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }
func (e *Encoder) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(e.w, "%s\n", b)
	return err
}

type Decoder struct{ s *bufio.Scanner }

func NewDecoder(r io.Reader) *Decoder {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 4<<20)
	return &Decoder{s: s}
}

var ErrInvalidMessage = errors.New("invalid engine message")

func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return false
	}
	switch value.(type) {
	case nil, string, json.Number:
		return true
	}
	return false
}

func (d *Decoder) Read(m *Message) error {
	*m = Message{}
	if !d.s.Scan() {
		if err := d.s.Err(); err != nil {
			return err
		}
		return io.EOF
	}
	if !json.Valid(d.s.Bytes()) {
		return &RPCError{Code: -32700, Message: "parse error"}
	}
	if err := json.Unmarshal(d.s.Bytes(), m); err != nil {
		return ErrInvalidMessage
	}
	if m.JSONRPC != "2.0" || !validID(m.ID) {
		return ErrInvalidMessage
	}
	if m.Method != "" {
		if len(m.Result) != 0 || m.Error != nil {
			return ErrInvalidMessage
		}
		if len(m.Params) != 0 && m.Params[0] != '{' && m.Params[0] != '[' {
			return ErrInvalidMessage
		}
	} else if len(m.ID) == 0 || (len(m.Result) != 0) == (m.Error != nil) || len(m.Params) != 0 {
		return ErrInvalidMessage
	}
	return nil
}

func (e *RPCError) Error() string { return e.Message }
