package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestControlSocketLifecycle(t *testing.T) {
	// Short Unix paths also work under macOS's 104-byte sockaddr limit.
	dir, err := os.MkdirTemp("/tmp", "aok-control-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s := supervisorForTest(t, filepath.Join(dir, "state"))
	path := filepath.Join(dir, "control.sock")
	l, err := NewControlListener(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewControlListener(path); err == nil {
		t.Fatal("active listener replaced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (&ControlServer{Supervisor: s, Listener: l}).Serve(ctx) }()
	c, err := DialControl(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("socket permissions", err)
	}
	var a Application
	if err = c.Call(ctx, "application.create", map[string]any{"owner_agent": "research"}, &a); err != nil {
		t.Fatal(err)
	}
	var applications []Application
	if err = c.Call(ctx, "application.list", map[string]any{}, &applications); err != nil || len(applications) != 1 || applications[0] != a {
		t.Fatal("application list", err)
	}
	if err = c.Call(ctx, "application.retire", map[string]any{"application_id": a.ApplicationID, "principal": "root"}, nil); err == nil {
		t.Fatal("spoofed principal accepted")
	}
	var inspected Application
	if err = c.Call(ctx, "application.inspect", map[string]string{"application_id": a.ApplicationID}, &inspected); err != nil || inspected != a {
		t.Fatal("inspect", err)
	}
	var m MailboxMessage
	if err = c.Call(ctx, "message.send", map[string]any{"application_id": a.ApplicationID, "idempotency_key": "one", "payload": map[string]string{"text": "hello"}}, &m); err != nil {
		t.Fatal(err)
	}
	var mailbox []MailboxMessage
	if err = c.Call(ctx, "mailbox.list", map[string]string{"application_id": a.ApplicationID}, &mailbox); err != nil || len(mailbox) != 1 || mailbox[0].MessageID != m.MessageID {
		t.Fatal("mailbox list", err)
	}
	var claimed []MailboxMessage
	if err = c.Call(ctx, "message.claim", map[string]string{"application_id": a.ApplicationID}, &claimed); err != nil || len(claimed) != 1 {
		t.Fatal("claim", err)
	}
	if err = c.Call(ctx, "message.ack", map[string]string{"application_id": a.ApplicationID, "message_id": m.MessageID}, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.Call(ctx, "event_source.create", map[string]any{"application_id": a.ApplicationID, "timer_id": "heartbeat", "delay_ms": 1000, "interval_ms": 0, "payload": map[string]bool{"tick": true}}, nil); err != nil {
		t.Fatal("timer create", err)
	}
	var timers []ApplicationTimer
	if err = c.Call(ctx, "event_source.list", map[string]string{"application_id": a.ApplicationID}, &timers); err != nil || len(timers) != 1 || timers[0].TimerID != "heartbeat" {
		t.Fatal("timer list", err)
	}
	source := map[string]any{"application_id": a.ApplicationID, "source_kind": "lsfs", "binding_id": "artifacts"}
	if err = c.Call(ctx, "event_source.create", source, nil); err != nil {
		t.Fatal("LSFS create", err)
	}
	if err = c.Call(ctx, "event_source.disable", source, nil); err != nil {
		t.Fatal("LSFS disable", err)
	}
	var imported struct{ Handle string }
	if err = c.Call(ctx, "artifact.import", map[string]any{"application_id": a.ApplicationID, "idempotency_key": "report", "payload": map[string]string{"text": "ready"}}, &imported); err != nil || imported.Handle == "" {
		t.Fatal("artifact import", err)
	}
	if err = c.Call(ctx, "event_source.bind", source, nil); err != nil {
		t.Fatal("LSFS enable", err)
	}
	if err = s.deliverLSFS(); err != nil {
		t.Fatal(err)
	}
	var bindings []WakeBinding
	if err = c.Call(ctx, "event_source.list", source, &bindings); err != nil || len(bindings) != 1 || !bindings[0].Enabled || bindings[0].Cursor == 0 || bindings[0].Ref != a.ContextID {
		t.Fatal("LSFS list", bindings, err)
	}
	if err = c.Call(ctx, "mailbox.list", map[string]string{"application_id": a.ApplicationID}, &mailbox); err != nil || len(mailbox) != 2 {
		t.Fatal("LSFS mailbox", mailbox, err)
	}
	if err = c.Call(ctx, "event_source.create", map[string]any{"application_id": a.ApplicationID, "source_kind": "port", "timer_id": "bad", "delay_ms": 1000, "payload": true}, nil); err == nil {
		t.Fatal("unsupported source accepted as timer")
	}
	var saturated Application
	if err = c.Call(ctx, "application.create", map[string]any{"owner_agent": "backpressure", "wake_policy": "manual"}, &saturated); err != nil {
		t.Fatal(err)
	}
	first := seedMailbox(t, s, []string{saturated.ApplicationID}, maxApplicationMailboxMessages, json.RawMessage(`{}`))[0]
	var capacity MailboxCapacity
	if err = c.Call(ctx, "mailbox.capacity", map[string]string{"application_id": saturated.ApplicationID}, &capacity); err != nil || capacity.Application.Messages != capacity.Application.MaxMessages || capacity.Supervisor.Messages != maxApplicationMailboxMessages+1 {
		t.Fatal("mailbox capacity", capacity, err)
	}
	retry := map[string]any{"application_id": saturated.ApplicationID, "idempotency_key": "retry", "payload": json.RawMessage(`{}`)}
	var rpc *RPCError
	if err = c.Call(ctx, "message.send", retry, nil); !errors.As(err, &rpc) || rpc.Code != -32005 {
		t.Fatal("missing retryable capacity error", err)
	}
	if err = c.Call(ctx, "message.claim", map[string]string{"application_id": saturated.ApplicationID}, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.Call(ctx, "message.ack", map[string]string{"application_id": saturated.ApplicationID, "message_id": first.MessageID}, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.Call(ctx, "message.send", retry, nil); err != nil {
		t.Fatal("connection or admission did not recover", err)
	}
	var verdict struct{ Allowed bool }
	if err = c.Call(ctx, "capability.check", map[string]string{"application_id": a.ApplicationID, "object": "net", "action": "connect"}, &verdict); err != nil || verdict.Allowed {
		t.Fatal("deny", err)
	}
	raw, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(time.Second))
	_, err = raw.Write([]byte("{bad json}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var response Message
	if err = NewDecoder(raw).Read(&response); err != nil || response.Error == nil || string(response.ID) != "null" {
		t.Fatal("malformed request", err)
	}
	var records []AuditRecord
	if err = c.Call(ctx, "audit.list", json.RawMessage(`{}`), &records); err != nil || VerifyAudit(records) != nil {
		t.Fatal("audit", err)
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown retained idle connections")
	}
}
