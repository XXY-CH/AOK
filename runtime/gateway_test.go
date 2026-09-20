package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The P4 gateway acceptance: a message enters the durable mailbox and
// wakes the application per its policy; the reply reaches the original
// conversation with a receipt; reconnects (duplicate deliver / duplicate
// claim / duplicate ack) never duplicate anything.
func TestGatewayRoundTripWithReconnects(t *testing.T) {
	s := newKernelTestSupervisor(t)
	s.policy.ExportMask = TaintExternal
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel("tester", "hook", "webhook"); err != nil {
		t.Fatal(err)
	}
	conversation, err := s.BindConversation("tester", "hook", "user-1", app.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	// Inbound at seq 1 lands in the durable mailbox.
	first, err := s.DeliverInbound("hook", "user-1", 1, json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.IdempotencyKey != "gw:hook:user-1:1" || first.Status != "pending" {
		t.Fatalf("inbound not routed to mailbox: %+v", first)
	}
	// Reconnect redelivery of the same sequence is a no-op: same message,
	// no second mailbox entry.
	again, err := s.DeliverInbound("hook", "user-1", 1, json.RawMessage(`{"text":"hello"}`))
	if err != nil || again.MessageID != first.MessageID {
		t.Fatalf("redelivery duplicated: %+v %v", again, err)
	}
	mailbox, _ := s.ListMailbox(app.ApplicationID)
	if len(mailbox) != 1 {
		t.Fatalf("mailbox has %d entries after redelivery", len(mailbox))
	}

	// The runner executes the turn (echo), and the reply is posted back to
	// the sourcing conversation with the result as the receipt carrier.
	if err := s.finishTurnForGateway(t, app.ApplicationID, first); err != nil {
		t.Fatal(err)
	}
	entry, err := s.Reply("tester", app.ApplicationID, first.MessageID)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ConversationID != "hook:user-1" || entry.Status != "pending" {
		t.Fatalf("reply misrouted: %+v", entry)
	}
	// Duplicate reply posts are deduplicated by idempotency.
	dup, err := s.Reply("tester", app.ApplicationID, first.MessageID)
	if err != nil || dup.EntryID != entry.EntryID {
		t.Fatalf("duplicate reply created a second entry: %+v %v", dup, err)
	}

	// Outbound delivery with reconnects: claim sees the pending entry,
	// a re-claim before ack sees it again (at-least-once to the remote),
	// and the remote dedups by idempotency key; ack is idempotent.
	claimed, err := s.ClaimOutbox("hook", "user-1")
	if err != nil || len(claimed) != 1 || claimed[0].EntryID != entry.EntryID || claimed[0].Attempts != 1 {
		t.Fatalf("claim wrong: %+v %v", claimed, err)
	}
	reclaimed, _ := s.ClaimOutbox("hook", "user-1")
	if len(reclaimed) != 1 || reclaimed[0].Attempts != 2 {
		t.Fatalf("re-claim before ack lost the entry: %+v", reclaimed)
	}
	if err := s.AckOutbox("tester", entry.EntryID, "remote-receipt-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AckOutbox("tester", entry.EntryID, "remote-receipt-1"); err != nil {
		t.Fatalf("duplicate ack not idempotent: %v", err)
	}
	if after, _ := s.ClaimOutbox("hook", "user-1"); len(after) != 0 {
		t.Fatalf("acked entry re-claimed: %+v", after)
	}

	// Sequence gating: an old sequence cannot be re-routed after the cursor
	// advanced, and a revoked channel refuses new deliveries.
	if _, err := s.DeliverInbound("hook", "user-1", 0, json.RawMessage(`{}`)); err == nil {
		t.Fatal("sequence 0 accepted")
	}
	if _, err := s.BindConversation("tester", "hook", "user-1", "other-app"); err == nil {
		t.Fatal("rebinding to a different application accepted")
	}
	if err := s.RevokeChannel("tester", "hook"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeliverInbound("hook", "user-1", 2, json.RawMessage(`{"text":"x"}`)); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("revoked channel accepted delivery: %v", err)
	}
	_ = conversation
}

// The wake-policy half of the acceptance: a manual application still
// accumulates pending gateway messages without a connected client, and an
// on_event application's turn is driven headless by the runner.
func TestGatewayHeadlessWake(t *testing.T) {
	s := newKernelTestSupervisor(t)
	manual, err := s.CreateApplication("tester", "owner", "manual")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel("tester", "im", "im"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversation("tester", "im", "u", manual.ApplicationID); err != nil {
		t.Fatal(err)
	}
	sent, err := s.DeliverInbound("im", "u", 1, json.RawMessage(`{"text":"queued"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Manual policy: the message waits in the mailbox (no client needed),
	// exactly the offline accumulation the gateway promises.
	mailbox, _ := s.ListMailbox(manual.ApplicationID)
	if len(mailbox) != 1 || mailbox[0].Status != "pending" {
		t.Fatalf("manual application lost its pending message: %+v", mailbox)
	}
	// Persistence: the conversation cursor and the pending message both
	// survive a restart, and the redelivery still dedups.
	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	redelivered, err := s2.DeliverInbound("im", "u", 1, json.RawMessage(`{"text":"queued"}`))
	if err != nil || redelivered.MessageID != sent.MessageID {
		t.Fatalf("restart redelivery not idempotent: %+v %v", redelivered, err)
	}
	conversations := s2.ListConversations("im")
	if len(conversations) != 1 || conversations[0].LastSequence != 1 {
		t.Fatalf("conversation cursor lost: %+v", conversations)
	}
}

// finishTurnForGateway drives one turn through the real finish path.
func (s *Supervisor) finishTurnForGateway(t *testing.T, applicationID string, m MailboxMessage) error {
	t.Helper()
	id, claimed, err := s.claimTurn()
	if err != nil || id != applicationID {
		return err
	}
	return s.finishTurn(id, claimed, "hello", Usage{InputTokens: 5, OutputTokens: 5},
		RouteInfo{Provider: "echo", CompatKey: "echo|std|plain|f32|1|cpu"}, nil)
}

// Two conversations on one application: the runner drives the turn from a
// gateway message (flat payload contract), the reply lands in the SOURCING
// conversation, and the ingress folded external taint.
func TestGatewayHeadlessRunAndSourcing(t *testing.T) {
	s := newKernelTestSupervisor(t)
	s.policy.ExportMask = TaintExternal
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"hook", "im"} {
		if _, err := s.CreateChannel("tester", ch, ch); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BindConversation("tester", ch, "user-1", app.ApplicationID); err != nil {
			t.Fatal(err)
		}
	}
	sent, err := s.DeliverInbound("im", "user-1", 1, json.RawMessage(`{"text":"the body"}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx, EchoProvider{}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Result(app.ApplicationID, sent.MessageID)
	if err != nil || result.Status != "completed" {
		t.Fatalf("headless gateway turn failed: %+v %v", result, err)
	}
	// The flat-payload contract: the model saw the actual body.
	if result.Text != "the body" {
		t.Fatalf("runner did not unwrap the gateway payload: %q", result.Text)
	}
	// The reply routes to the sourcing conversation, not the other one.
	entry, err := s.Reply("tester", app.ApplicationID, sent.MessageID)
	if err != nil || entry.ConversationID != "im:user-1" {
		t.Fatalf("reply misrouted: %+v %v", entry, err)
	}
	if other, _ := s.ClaimOutbox("hook", "user-1"); len(other) != 0 {
		t.Fatalf("reply leaked to the non-sourcing conversation: %+v", other)
	}
	// Gateway ingress is externally tainted by contract.
	inspected, _ := s.InspectApplication(app.ApplicationID)
	if inspected.TaintBits&TaintExternal == 0 {
		t.Fatal("gateway ingress folded no external taint")
	}
	// Re-binding the same identity preserves the cursor (no replay window).
	before := s.ListConversations("im")
	if _, err := s.BindConversation("tester", "im", "user-1", app.ApplicationID); err != nil {
		t.Fatal(err)
	}
	after := s.ListConversations("im")
	if after[0].LastSequence != before[0].LastSequence || after[0].CreatedAt != before[0].CreatedAt {
		t.Fatalf("re-bind wiped the cursor: %+v -> %+v", before[0], after[0])
	}
}

// Replies must survive a real restart: the sourcing map is rebuilt from the
// durable envelopes on load, and it is per-supervisor (no cross-instance
// message-id collisions).
func TestReplySurvivesRestart(t *testing.T) {
	s := newKernelTestSupervisor(t)
	s.policy.ExportMask = TaintExternal
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel("tester", "im", "im"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversation("tester", "im", "u", app.ApplicationID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeliverInbound("im", "u", 1, json.RawMessage(`{"text":"x"}`)); err != nil {
		t.Fatal(err)
	}
	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Fresh instance: the sourcing map must be rebuilt from the envelope.
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo", ExportMask: TaintExternal})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// The turn result exists via the runner path or a direct finish; here
	// the message is still pending — reply for an unexecuted message is
	// rejected before the routing check anyway. Execute it first.
	id, m, err := s2.claimTurn()
	if err != nil || id != app.ApplicationID {
		t.Fatalf("claim: %v", err)
	}
	if err := s2.finishTurn(id, m, "x", Usage{InputTokens: 1, OutputTokens: 1},
		RouteInfo{Provider: "echo"}, nil); err != nil {
		t.Fatal(err)
	}
	entry, err := s2.Reply("tester", app.ApplicationID, m.MessageID)
	if err != nil || entry.ConversationID != "im:u" {
		t.Fatalf("reply after restart lost its sourcing: %+v %v", entry, err)
	}
	// Per-instance isolation: a second supervisor with its own root must
	// not see the first one's sourcing (colliding msg-N ids).
	s3 := newKernelTestSupervisor(t)
	s3.policy.ExportMask = TaintExternal
	defer s3.Close()
	app3, _ := s3.CreateApplication("tester", "owner", "on_event")
	if _, err := s3.CreateChannel("tester", "other", "other"); err != nil {
		t.Fatal(err)
	}
	if _, err := s3.BindConversation("tester", "other", "v", app3.ApplicationID); err != nil {
		t.Fatal(err)
	}
	sent3, err := s3.DeliverInbound("other", "v", 1, json.RawMessage(`{"text":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	if id3, m3, err := s3.claimTurn(); err != nil || id3 != app3.ApplicationID {
		t.Fatalf("claim3: %v", err)
	} else if err := s3.finishTurn(id3, m3, "y", Usage{InputTokens: 1}, RouteInfo{Provider: "echo"}, nil); err != nil {
		t.Fatal(err)
	}
	entry3, err := s3.Reply("tester", app3.ApplicationID, sent3.MessageID)
	if err != nil || entry3.ConversationID != "other:v" {
		t.Fatalf("cross-instance sourcing corrupted: %+v %v", entry3, err)
	}
	// The first supervisor's routing is unaffected by the second's writes.
	again, err := s2.Reply("tester", app.ApplicationID, m.MessageID)
	if err == nil && again.EntryID != entry.EntryID {
		// dedup should return the SAME entry, not create one under the other instance's map
		t.Fatalf("dedup broken after cross-instance activity: %+v", again)
	}
}

func TestReplyRestartRoutesEachConversationExactly(t *testing.T) {
	s := newKernelTestSupervisor(t)
	s.policy.ExportMask = TaintExternal
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel("tester", "im", "im"); err != nil {
		t.Fatal(err)
	}
	externalIDs := []string{"alice", "bob", "carol", "dave"}
	messages := make([]MailboxMessage, 0, len(externalIDs))
	for _, externalID := range externalIDs {
		if _, err := s.BindConversation("tester", "im", externalID, app.ApplicationID); err != nil {
			t.Fatal(err)
		}
		message, err := s.DeliverInbound("im", externalID, 1,
			json.RawMessage(`{"text":"hello"}`))
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	for range messages {
		id, message, err := s.claimTurn()
		if err != nil || id != app.ApplicationID {
			t.Fatalf("claim: id=%q err=%v", id, err)
		}
		if err := s.finishTurn(id, message, "ok", Usage{InputTokens: 1},
			RouteInfo{Provider: "echo"}, nil); err != nil {
			t.Fatal(err)
		}
	}

	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo", ExportMask: TaintExternal})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	for i, message := range messages {
		entry, err := s2.Reply("tester", app.ApplicationID, message.MessageID)
		want := conversationKey("im", externalIDs[i])
		if err != nil || entry.ConversationID != want {
			t.Fatalf("message %s routed to %q, want %q: %v",
				message.MessageID, entry.ConversationID, want, err)
		}
	}
}

func TestReplyBlocksConfidentialTaintAndPersistsDeny(t *testing.T) {
	s := newKernelTestSupervisor(t)
	s.policy.ExportMask = TaintExternal
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel("tester", "im", "im"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversation("tester", "im", "u", app.ApplicationID); err != nil {
		t.Fatal(err)
	}
	message, err := s.DeliverInbound("im", "u", 1, json.RawMessage(`{"text":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.finishTurnForGateway(t, app.ApplicationID, message); err != nil {
		t.Fatal(err)
	}
	s.accumulateTaint(app.ApplicationID, TaintConfidential)
	if _, err := s.Reply("tester", app.ApplicationID, message.MessageID); !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("confidential reply was not blocked: %v", err)
	}
	if len(s.state.Outbox) != 0 {
		t.Fatalf("blocked reply entered outbox: %+v", s.state.Outbox)
	}
	audit := s.Audit()
	last := audit[len(audit)-1]
	if last.Action != "gateway.reply" || last.Decision != "deny" || last.Reason != "unmasked taint" {
		t.Fatalf("missing reply deny audit: %+v", last)
	}

	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo", ExportMask: TaintExternal})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	audit = s2.Audit()
	found := false
	for _, record := range audit {
		if record.Action == "gateway.reply" && record.Object == message.MessageID &&
			record.Decision == "deny" && record.Reason == "unmasked taint" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("reply deny was not persisted")
	}
}

func TestRevokedChannelBlocksReplyAndOutboxClaim(t *testing.T) {
	s := newKernelTestSupervisor(t)
	s.policy.ExportMask = TaintExternal
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel("tester", "im", "im"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindConversation("tester", "im", "u", app.ApplicationID); err != nil {
		t.Fatal(err)
	}
	var messages []MailboxMessage
	for sequence := int64(1); sequence <= 2; sequence++ {
		message, err := s.DeliverInbound("im", "u", sequence, json.RawMessage(`{"text":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	for range messages {
		id, message, err := s.claimTurn()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.finishTurn(id, message, "ok", Usage{InputTokens: 1},
			RouteInfo{Provider: "echo"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Reply("tester", app.ApplicationID, messages[0].MessageID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeChannel("tester", "im"); err != nil {
		t.Fatal(err)
	}
	if claimed, err := s.ClaimOutbox("im", "u"); !errors.Is(err, ErrChannelNotFound) || len(claimed) != 0 {
		t.Fatalf("revoked channel claimed outbox: %+v %v", claimed, err)
	}
	if _, err := s.Reply("tester", app.ApplicationID, messages[1].MessageID); !errors.Is(err, ErrChannelNotFound) {
		t.Fatalf("revoked channel accepted reply: %v", err)
	}
}
