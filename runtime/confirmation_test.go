package runtime

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// The unotify-equivalent slow path: an unmasked-taint export escalates to a
// pending confirmation, an approver settles it, and the approved request
// releases exactly one export before being consumed.
func TestConfirmationSlowPath(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("tester", app.ApplicationID, "t",
		json.RawMessage(`{"text":"x","taint":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.claimTurn(); err != nil {
		t.Fatal(err)
	}

	// Escalation instead of a synchronous deny.
	_, err = s.exportApplication("tester", app.ApplicationID, "leak", false, true, "")
	if !errors.Is(err, ErrConfirmationPending) {
		t.Fatalf("escalation not offered: %v", err)
	}
	pending := s.ListConfirmations(app.ApplicationID)
	if len(pending) != 1 || pending[0].Status != "pending" ||
		pending[0].Kind != "taint.export" {
		t.Fatalf("pending request wrong: %+v", pending)
	}
	requestID := pending[0].RequestID

	// Before approval the request does not release anything.
	if _, err := s.exportApplication("tester", app.ApplicationID, "leak", false, false, requestID); err == nil {
		t.Fatal("unapproved request released an export")
	}

	// Deny settles it; the request can never release an export afterwards.
	if _, err := s.SettleConfirmation("human", requestID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.exportApplication("tester", app.ApplicationID, "leak", false, false, requestID); err == nil {
		t.Fatal("denied request released an export")
	}
	if _, err := s.SettleConfirmation("human", requestID, true); err == nil {
		t.Fatal("re-settling an already settled request accepted")
	}

	// A fresh request, approved this time, releases exactly one export.
	_, err = s.exportApplication("tester", app.ApplicationID, "leak2", false, true, "")
	if !errors.Is(err, ErrConfirmationPending) {
		t.Fatalf("second escalation failed: %v", err)
	}
	pending = s.ListConfirmations(app.ApplicationID)
	var approvedID string
	for _, request := range pending {
		if request.Status == "pending" {
			approvedID = request.RequestID
		}
	}
	if approvedID == "" {
		t.Fatal("second pending request missing")
	}
	if _, err := s.SettleConfirmation("human", approvedID, true); err != nil {
		t.Fatal(err)
	}
	if text, err := s.exportApplication("tester", app.ApplicationID, "leak2", false, false, approvedID); err != nil || text != "leak2" {
		t.Fatalf("approved request did not release export: %q %v", text, err)
	}
	// One-shot: the same request cannot release a second export.
	if _, err := s.exportApplication("tester", app.ApplicationID, "leak3", false, false, approvedID); err == nil ||
		!strings.Contains(err.Error(), "already used") {
		t.Fatalf("consumed request reused: %v", err)
	}

	// Requests survive restart; a pending one can still be settled after.
	_, err = s.exportApplication("tester", app.ApplicationID, "leak4", false, true, "")
	if !errors.Is(err, ErrConfirmationPending) {
		t.Fatal(err)
	}
	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	restarted := s2.ListConfirmations(app.ApplicationID)
	var revivedID string
	stillConsumed, stillDenied := false, false
	for _, request := range restarted {
		switch request.Status {
		case "pending":
			revivedID = request.RequestID
		case "consumed":
			stillConsumed = true
		case "denied":
			stillDenied = true
		}
	}
	if revivedID == "" || !stillConsumed || !stillDenied {
		t.Fatalf("confirmation states lost across restart: %+v", restarted)
	}
	if _, err := s2.SettleConfirmation("human", revivedID, true); err != nil {
		t.Fatalf("revived request not settleable: %v", err)
	}
	// Audit trail covers create, deny, approve and the one-shot release.
	actions := map[string]bool{}
	for _, r := range s2.Audit() {
		actions[r.Action] = true
	}
	for _, want := range []string{"confirmation.create", "confirmation.approve", "confirmation.deny"} {
		if !actions[want] {
			t.Fatalf("audit missing %s", want)
		}
	}
}
