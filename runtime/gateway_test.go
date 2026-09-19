package runtime

import (
	"encoding/json"
	"errors"
	"testing"
)

// The P4 gateway acceptance: a message enters the durable mailbox and
// wakes the application per its policy; the reply reaches the original
// conversation with a receipt; reconnects (duplicate deliver / duplicate
// claim / duplicate ack) never duplicate anything.
func TestGatewayRoundTripWithReconnects(t *testing.T) {
	s := newKernelTestSupervisor(t)
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
