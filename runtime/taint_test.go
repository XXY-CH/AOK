package runtime

import (
	"encoding/json"
	"errors"
	"testing"
)

// The P3 taint gate: payloads declare taint, the application accumulates it
// through turns that consume tainted input, and exports reject unmasked
// bits unless explicitly confirmed (which is itself audited).
func TestTaintGateBlocksAndConfirms(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Clean application exports freely.
	if _, err := s.exportApplication("tester", app.ApplicationID, "clean", false); err != nil {
		t.Fatalf("clean export blocked: %v", err)
	}
	// A tainted message taints the application at claim time.
	if _, err := s.Enqueue("tester", app.ApplicationID, "t1",
		json.RawMessage(`{"text":"secret","taint":3}`)); err != nil {
		t.Fatal(err)
	}
	if id, _, err := s.claimTurn(); err != nil || id == "" {
		t.Fatalf("claim: %v", err)
	}
	inspected, _ := s.InspectApplication(app.ApplicationID)
	if inspected.TaintBits != 3 {
		t.Fatalf("taint not accumulated: %d", inspected.TaintBits)
	}
	// Export mask is zero by default: both bits are unmasked.
	_, err = s.exportApplication("tester", app.ApplicationID, "leak", false)
	if !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("tainted export not blocked: %v", err)
	}
	// Explicit confirmation downgrades the block to an audited approval.
	if _, err := s.exportApplication("tester", app.ApplicationID, "leak", true); err != nil {
		t.Fatalf("confirmed export failed: %v", err)
	}
	audit := s.Audit()
	foundDeny, foundConfirm := false, false
	for _, r := range audit {
		if r.Action == "taint.export" && r.Decision == "deny" {
			foundDeny = true
		}
		if r.Action == "taint.export.confirm" && r.Decision == "allow" {
			foundConfirm = true
		}
	}
	if !foundDeny || !foundConfirm {
		t.Fatalf("gate decisions not audited: deny=%v confirm=%v", foundDeny, foundConfirm)
	}
}

func TestTaintGateHonorsExportMask(t *testing.T) {
	s := newKernelTestSupervisor(t)
	s.policy.ExportMask = TaintExternal // confidential bit stays masked
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("tester", app.ApplicationID, "t",
		json.RawMessage(`{"text":"x","taint":3}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.claimTurn(); err != nil {
		t.Fatal(err)
	}
	inspected, _ := s.InspectApplication(app.ApplicationID)
	if inspected.TaintBits != 3 {
		t.Fatalf("taint: %d", inspected.TaintBits)
	}
	// TaintExternal (bit 0) is masked; TaintConfidential (bit 1) is not.
	if _, err := s.exportApplication("tester", app.ApplicationID, "x", false); !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("unmasked confidential bit exported: %v", err)
	}
	// Restart keeps the taint ledger and the gate behavior.
	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo", ExportMask: TaintExternal})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.exportApplication("tester", app.ApplicationID, "x", false); !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("taint ledger lost across restart: %v", err)
	}
	if _, err := s2.exportApplication("tester", app.ApplicationID, "x", true); err != nil {
		t.Fatalf("confirm after restart failed: %v", err)
	}
}
