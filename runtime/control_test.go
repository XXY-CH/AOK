package runtime

import (
	"context"
	"encoding/json"
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
