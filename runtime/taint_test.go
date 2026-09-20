package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
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
	if _, err := s.exportApplication("tester", app.ApplicationID, "clean", false, false, ""); err != nil {
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
	_, err = s.exportApplication("tester", app.ApplicationID, "leak", false, false, "")
	if !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("tainted export not blocked: %v", err)
	}
	// Explicit confirmation downgrades the block to an audited approval.
	if _, err := s.exportApplication("tester", app.ApplicationID, "leak", true, false, ""); err != nil {
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
	if _, err := s.exportApplication("tester", app.ApplicationID, "x", false, false, ""); !errors.Is(err, ErrTaintBlocked) {
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
	if _, err := s2.exportApplication("tester", app.ApplicationID, "x", false, false, ""); !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("taint ledger lost across restart: %v", err)
	}
	if _, err := s2.exportApplication("tester", app.ApplicationID, "x", true, false, ""); err != nil {
		t.Fatalf("confirm after restart failed: %v", err)
	}
}

// A provider that reports kernel-style session taint: the runner must fold
// it into the application ledger, and the export gate must then block. The
// taint travels with the call (TrackedProvider); latest-wins getters only
// work for serial execution.
type taintedEcho struct{ taint uint64 }

func (p *taintedEcho) Name() string { return "tainted-echo" }
func (p *taintedEcho) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	text, usage, _, _, err := p.CompleteTracked(ctx, prompt)
	return text, usage, err
}
func (p *taintedEcho) CompleteTracked(ctx context.Context, prompt string) (string, Usage, RouteInfo, uint64, error) {
	return prompt, Usage{InputTokens: uint64(len(prompt)), OutputTokens: uint64(len(prompt))},
		RouteInfo{Provider: p.Name()}, p.taint, nil
}

func TestProviderTaintFlowsToExportGate(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("tester", app.ApplicationID, "k",
		json.RawMessage(`{"text":"clean"}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// The input is untainted; the provider labels its result with session
	// taint — the ledger must pick it up and the gate must block.
	if err := s.Run(ctx, &taintedEcho{taint: TaintConfidential}); err != nil {
		t.Fatalf("run: %v", err)
	}
	inspected, _ := s.InspectApplication(app.ApplicationID)
	if inspected.TaintBits&TaintConfidential == 0 {
		t.Fatalf("provider taint not folded: %d", inspected.TaintBits)
	}
	if _, err := s.exportApplication("tester", app.ApplicationID, "x", false, false, ""); !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("gate did not see provider taint: %v", err)
	}
}

// The control-plane claim path must fold taint exactly like the runner:
// consuming a tainted payload via message.claim gates later exports.
func TestClaimMailboxFoldsTaint(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("tester", app.ApplicationID, "t",
		json.RawMessage(`{"text":"x","taint":1}`)); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimMailbox("tester", app.ApplicationID)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %+v %v", claimed, err)
	}
	inspected, _ := s.InspectApplication(app.ApplicationID)
	if inspected.TaintBits != 1 {
		t.Fatalf("control-plane claim skipped the taint fold: %d", inspected.TaintBits)
	}
	if _, err := s.exportApplication("tester", app.ApplicationID, "x", false, false, ""); !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("RPC-claimed taint not gated: %v", err)
	}
}
