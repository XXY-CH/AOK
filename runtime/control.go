package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ControlServer is the local supervisor control plane. It deliberately uses a
// separate listener from the engine protocol: control methods carry principal
// and audit correlation, while engine sessions carry model turns.
type ControlServer struct {
	Supervisor *Supervisor
	Listener   net.Listener
}

func (s *ControlServer) Serve(ctx context.Context) error {
	if s.Supervisor == nil || s.Listener == nil {
		return errors.New("control server requires supervisor and listener")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer s.Listener.Close()
	stop := context.AfterFunc(ctx, func() { s.Listener.Close() })
	defer stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	defer cancel()
	slots := make(chan struct{}, maxConnections)
	for {
		conn, err := s.Listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			workers.Add(1)
			go func() { defer workers.Done(); defer func() { <-slots }(); s.serveConnection(ctx, conn) }()
		default:
			conn.Close()
		}
	}
}

func (s *ControlServer) serveConnection(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	principal, err := controlPrincipal(conn)
	if err != nil {
		return
	}
	decoder := NewDecoder(conn)
	encoder := NewEncoder(conn)
	for {
		var req Message
		if err := decoder.Read(&req); err != nil {
			var rpc *RPCError
			if errors.As(err, &rpc) || errors.Is(err, ErrInvalidMessage) {
				_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if encoder.Write(Message{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &RPCError{Code: -32600, Message: "invalid request"}}) != nil {
					return
				}
				continue
			}
			return
		}
		if req.Method == "" {
			return
		}
		resp := s.handle(req, principal)
		if len(req.ID) > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if err := encoder.Write(resp); err != nil {
				return
			}
		}
	}
}

func (s *ControlServer) handle(req Message, principal string) Message {
	resp := Message{JSONRPC: "2.0", ID: req.ID}
	var p struct {
		Principal     string          `json:"principal"`
		ApplicationID string          `json:"application_id"`
		OwnerAgent    string          `json:"owner_agent"`
		WakePolicy    string          `json:"wake_policy"`
		Key           string          `json:"idempotency_key"`
		Payload       json.RawMessage `json:"payload"`
		MessageID     string          `json:"message_id"`
		Object        string          `json:"object"`
		Action        string          `json:"action"`
		Path          string          `json:"path"`
		Token         string          `json:"capability_token"`
		TokenLimit    uint64          `json:"token_limit"`
		TimerID       string          `json:"timer_id"`
		DelayMS       int64           `json:"delay_ms"`
		IntervalMS    int64           `json:"interval_ms"`
		SourceKind    string          `json:"source_kind"`
		SourceRef     string          `json:"source_ref"`
		BindingID     string          `json:"binding_id"`
		AfterCursor   int64           `json:"after_cursor"`
		Backends      []string        `json:"backends"`
		Text          string          `json:"text"`
		Confirm       bool            `json:"confirm"`
		Escalate      bool            `json:"escalate"`
		RequestID     string          `json:"request_id"`
		Approve       bool            `json:"approve"`
		HeadHash      string          `json:"head_hash"`
		Count         uint64          `json:"count"`
		Time          int64           `json:"time"`
		PublicKey     json.RawMessage `json:"public_key"`
		Signature     json.RawMessage `json:"signature"`
		Reason        string          `json:"reason"`
	}
	if len(req.Params) > 0 && json.Unmarshal(req.Params, &p) != nil {
		resp.Error = &RPCError{Code: -32602, Message: "invalid params"}
		return resp
	}
	if p.Principal != "" && p.Principal != principal {
		resp.Error = &RPCError{Code: -32003, Message: "principal does not match peer identity"}
		return resp
	}
	p.Principal = principal
	switch req.Method {
	case "health":
		resp.Result = json.RawMessage(`{"status":"ok","component":"supervisor"}`)
	case "application.create":
		a, err := s.Supervisor.CreateApplication(p.Principal, p.OwnerAgent, p.WakePolicy)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result, _ = json.Marshal(a)
		}
	case "application.inspect":
		a, err := s.Supervisor.InspectApplication(p.ApplicationID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result, _ = json.Marshal(a)
		}
	case "application.list":
		resp.Result, _ = json.Marshal(s.Supervisor.ListApplications())
	case "application.retire":
		err := s.Supervisor.RetireApplication(p.Principal, p.ApplicationID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"retired":true}`)
		}
	case "application.freeze", "application.resume":
		state := "frozen"
		if req.Method == "application.resume" {
			state = "serving"
		}
		if err := s.Supervisor.SetApplicationState(p.Principal, p.ApplicationID, state); err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"ok":true}`)
		}
	case "application.set_route_policy":
		policy, err := s.Supervisor.SetRoutePolicy(p.Principal, p.ApplicationID, p.Backends)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else if raw, merr := json.Marshal(policy); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "route.list":
		records, err := s.Supervisor.RouteRecords(p.ApplicationID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else if raw, merr := json.Marshal(records); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "application.export":
		if _, err := s.Supervisor.exportApplication(p.Principal, p.ApplicationID, p.Text, p.Confirm, p.Escalate, p.RequestID); err != nil {
			code := -32000
			if errors.Is(err, ErrTaintBlocked) {
				code = -32006
			}
			if errors.Is(err, ErrConfirmationPending) {
				code = -32007
			}
			resp.Error = &RPCError{Code: code, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"exported":true}`)
		}
	case "witness.head":
		head, count, err := s.Supervisor.WitnessHead()
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			break
		}
		if raw, merr := json.Marshal(map[string]any{"head_hash": head, "count": count}); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "witness.cosign":
		var cp WitnessCheckpoint
		if raw, merr := json.Marshal(map[string]any{
			"head_hash": p.HeadHash, "count": p.Count, "time": p.Time,
			"public_key": p.PublicKey, "signature": p.Signature, "signer": p.Reason,
		}); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
			break
		} else if uerr := json.Unmarshal(raw, &cp); uerr != nil {
			resp.Error = &RPCError{Code: -32002, Message: uerr.Error()}
			break
		}
		if err := s.Supervisor.RecordCosignature(p.Principal, cp); err != nil {
			code := -32000
			if errors.Is(err, ErrWitnessMismatch) {
				code = -32008
			}
			resp.Error = &RPCError{Code: code, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"witnessed":true}`)
		}
	case "witness.verify":
		if err := s.Supervisor.VerifyWitness(); err != nil {
			resp.Error = &RPCError{Code: -32008, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"verified":true}`)
		}
	case "message.channel.create":
		channel, err := s.Supervisor.CreateChannel(p.Principal, p.BindingID, p.SourceKind)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else if raw, merr := json.Marshal(channel); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "message.channel.revoke":
		if err := s.Supervisor.RevokeChannel(p.Principal, p.BindingID); err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"revoked":true}`)
		}
	case "message.conversation.bind":
		conversation, err := s.Supervisor.BindConversation(p.Principal, p.BindingID, p.Path, p.ApplicationID)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else if raw, merr := json.Marshal(conversation); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "message.conversation.list":
		if raw, merr := json.Marshal(s.Supervisor.ListConversations(p.BindingID)); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "gateway.deliver":
		if p.Count <= 0 {
			resp.Error = &RPCError{Code: -32002, Message: "sequence required"}
			break
		}
		message, err := s.Supervisor.DeliverInbound(p.BindingID, p.Path, int64(p.Count), p.Payload)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else if raw, merr := json.Marshal(message); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "gateway.reply":
		entry, err := s.Supervisor.Reply(p.Principal, p.ApplicationID, p.MessageID)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else if raw, merr := json.Marshal(entry); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "gateway.outbox.claim":
		entries, err := s.Supervisor.ClaimOutbox(p.BindingID, p.Path)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else if raw, merr := json.Marshal(entries); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "gateway.outbox.ack":
		if err := s.Supervisor.AckOutbox(p.Principal, p.MessageID, p.Reason); err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"acked":true}`)
		}
	case "confirmation.list":
		requests := s.Supervisor.ListConfirmations(p.ApplicationID)
		if raw, merr := json.Marshal(requests); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "confirmation.settle":
		request, err := s.Supervisor.SettleConfirmation(p.Principal, p.RequestID, p.Approve)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else if raw, merr := json.Marshal(request); merr != nil {
			resp.Error = &RPCError{Code: -32000, Message: merr.Error()}
		} else {
			resp.Result = raw
		}
	case "budget.set":
		if err := s.Supervisor.SetTokenLimit(p.Principal, p.ApplicationID, p.TokenLimit); err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"ok":true}`)
		}
	case "event_source.create":
		if p.SourceKind == "lsfs" {
			if err := s.Supervisor.AddLSFSBinding(p.Principal, p.ApplicationID, p.BindingID, p.SourceRef, p.AfterCursor); err != nil {
				resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			} else {
				resp.Result = json.RawMessage(`{"ok":true}`)
			}
			break
		}
		if p.SourceKind != "" && p.SourceKind != "timer" {
			resp.Error = &RPCError{Code: -32602, Message: "unsupported source kind"}
			break
		}
		if p.DelayMS <= 0 || p.DelayMS > 86400000 || p.IntervalMS < 0 || p.IntervalMS > 86400000 {
			resp.Error = &RPCError{Code: -32602, Message: "timer duration out of range"}
			break
		}
		if err := s.Supervisor.AddTimer(p.Principal, p.ApplicationID, p.TimerID, time.Duration(p.DelayMS)*time.Millisecond, time.Duration(p.IntervalMS)*time.Millisecond, p.Payload); err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"ok":true}`)
		}
	case "message.result":
		r, err := s.Supervisor.Result(p.ApplicationID, p.MessageID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result, _ = json.Marshal(r)
		}
	case "message.send":
		m, err := s.Supervisor.Enqueue(p.Principal, p.ApplicationID, p.Key, p.Payload)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
			if errors.Is(err, ErrMailboxFull) {
				resp.Error.Code = -32005
			}
		} else {
			resp.Result, _ = json.Marshal(m)
		}
	case "message.claim":
		m, err := s.Supervisor.ClaimMailbox(p.Principal, p.ApplicationID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result, _ = json.Marshal(m)
		}
	case "mailbox.capacity":
		capacity, err := s.Supervisor.InspectMailboxCapacity(p.ApplicationID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result, _ = json.Marshal(capacity)
		}
	case "mailbox.list":
		m, err := s.Supervisor.ListMailbox(p.ApplicationID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result, _ = json.Marshal(m)
		}
	case "event_source.list":
		if p.SourceKind == "lsfs" {
			bindings, err := s.Supervisor.ListLSFSBindings(p.ApplicationID)
			if err != nil {
				resp.Error = &RPCError{Code: -32004, Message: err.Error()}
			} else {
				resp.Result, _ = json.Marshal(bindings)
			}
			break
		}
		if p.SourceKind != "" && p.SourceKind != "timer" {
			resp.Error = &RPCError{Code: -32602, Message: "unsupported source kind"}
			break
		}
		timers, err := s.Supervisor.ListTimers(p.ApplicationID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result, _ = json.Marshal(timers)
		}
	case "event_source.bind", "event_source.disable":
		if p.SourceKind != "lsfs" {
			resp.Error = &RPCError{Code: -32602, Message: "bind/disable requires lsfs source kind"}
		} else if err := s.Supervisor.SetLSFSBindingEnabled(p.Principal, p.ApplicationID, p.BindingID, req.Method == "event_source.bind"); err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"ok":true}`)
		}
	case "artifact.import":
		handle, err := s.Supervisor.ImportArtifact(p.Principal, p.ApplicationID, p.Key, p.Payload)
		if err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result, _ = json.Marshal(map[string]string{"handle": handle})
		}
	case "message.ack":
		err := s.Supervisor.AckMailbox(p.Principal, p.ApplicationID, p.MessageID)
		if err != nil {
			resp.Error = &RPCError{Code: -32004, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"acked":true}`)
		}
	case "capability.revoke":
		var token CapabilityToken
		if p.Token != "" {
			parsed, perr := ParseCapabilityToken(p.Token)
			if perr != nil {
				resp.Error = &RPCError{Code: -32002, Message: perr.Error()}
				break
			}
			token = parsed
		}
		if err := s.Supervisor.RevokeCapabilityToken(p.Principal, token, p.Reason); err != nil {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else {
			resp.Result = json.RawMessage(`{"revoked":true}`)
		}
	case "capability.check":
		if p.Path != "" {
			p.Action = p.Path
		}
		var token *CapabilityToken
		if p.Token != "" {
			parsed, parseErr := ParseCapabilityToken(p.Token)
			if parseErr != nil {
				resp.Result, _ = json.Marshal(map[string]any{"allowed": false, "reason": parseErr.Error()})
				break
			}
			token = &parsed
		}
		var ok bool
		var err error
		if token == nil {
			ok, err = s.Supervisor.Check(p.Principal, p.ApplicationID, p.Object, p.Action)
		} else {
			ok, err = s.Supervisor.CheckWithToken(p.Principal, p.ApplicationID, p.Object, p.Action, *token)
		}
		if err != nil && !errors.Is(err, ErrCapabilityDenied) {
			resp.Error = &RPCError{Code: -32000, Message: err.Error()}
		} else if err != nil {
			resp.Result, _ = json.Marshal(map[string]any{"allowed": false, "reason": err.Error()})
		} else {
			resp.Result, _ = json.Marshal(map[string]any{"allowed": ok})
		}
	case "audit.list":
		resp.Result, _ = json.Marshal(s.Supervisor.Audit())
	default:
		resp.Error = &RPCError{Code: -32601, Message: "method not found"}
	}
	return resp
}

func NewControlListener(path string) (net.Listener, error) {
	if path == "" {
		return nil, errors.New("control socket path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("control socket requires private parent directory")
	}
	// Bind must fail on any existing path. Never unlink another server's socket.
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

// ControlClient is a small local adapter used by tests and the host CLI.
type ControlClient struct {
	mu       sync.Mutex
	sequence uint64
	Conn     net.Conn
	Encoder  *Encoder
	Decoder  *Decoder
}

func DialControl(path string) (*ControlClient, error) {
	c, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return &ControlClient{Conn: c, Encoder: NewEncoder(c), Decoder: NewDecoder(c)}, nil
}

func (c *ControlClient) Call(ctx context.Context, method string, params, result any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok {
		deadline = d
	}
	if err := c.Conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { c.Conn.Close() })
	defer stop()
	c.sequence++
	id := json.RawMessage(fmt.Sprint(c.sequence))
	p, err := json.Marshal(params)
	if err != nil {
		return err
	}
	if err = c.Encoder.Write(Message{JSONRPC: "2.0", ID: id, Method: method, Params: p}); err != nil {
		return err
	}
	var response Message
	if err = c.Decoder.Read(&response); err != nil {
		return err
	}
	if string(response.ID) != string(id) {
		return errors.New("control response ID mismatch")
	}
	if response.Error != nil {
		return response.Error
	}
	if result != nil {
		return json.Unmarshal(response.Result, result)
	}
	return nil
}

func (c *ControlClient) Close() error { return c.Conn.Close() }
