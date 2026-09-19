package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRouterPolicyFiltersBackends(t *testing.T) {
	r, err := NewRouterProvider(failingProvider{"llama.cpp"},
		fixedProvider{"anthropic", "ok"}, fixedProvider{"echo", "no"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRouteBackends(context.Background(), []string{"echo"})
	text, _, err := r.Complete(ctx, "p")
	if err != nil || text != "no" {
		t.Fatalf("policy routing wrong: %q %v", text, err)
	}
	if route := r.LastRoute(); route.Provider != "echo" || route.Fallbacks != 0 {
		t.Fatalf("policy-skipped providers must not count as fallbacks: %+v", route)
	}
	if _, _, err = r.Complete(WithRouteBackends(context.Background(), []string{"absent"}), "p"); err == nil ||
		"route policy excludes every backend" != err.Error() {
		t.Fatalf("empty candidate set must be denied: %v", err)
	}
	// Unrestricted context still falls back past the failing provider.
	if text, _, err = r.Complete(context.Background(), "p"); err != nil || text != "ok" {
		t.Fatalf("unrestricted routing broken: %q %v", text, err)
	}
}

func TestRouterFallbackCountExcludesPolicySkips(t *testing.T) {
	r, err := NewRouterProvider(failingProvider{"llama.cpp"}, failingProvider{"metal"},
		fixedProvider{"anthropic", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithRouteBackends(context.Background(), []string{"llama.cpp", "anthropic"})
	if _, _, err = r.Complete(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if route := r.LastRoute(); route.Provider != "anthropic" || route.Fallbacks != 1 {
		t.Fatalf("fallback count must exclude the policy-skipped metal: %+v", route)
	}
}

func TestRoutePolicyDeniesAndRecords(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	policy, err := s.SetRoutePolicy("tester", app.ApplicationID, []string{"llama.cpp"})
	if err != nil || policy.Version != 1 {
		t.Fatalf("set policy: %+v %v", policy, err)
	}
	if _, err := s.Enqueue("tester", app.ApplicationID, "k1",
		json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	id, m, err := s.claimTurn()
	if err != nil || id == "" {
		t.Fatalf("claim: %v", err)
	}
	// The echo provider is not in the policy: the turn must be denied
	// before any backend call and recorded with the denial reason.
	if err := s.finishTurn(id, m, "", Usage{}, RouteInfo{Provider: "echo"}, ErrRouteDenied); err != nil {
		t.Fatalf("finishTurn: %v", err)
	}
	result, _ := s.Result(app.ApplicationID, m.MessageID)
	if result.Status != "failed" {
		t.Fatalf("denied turn not failed: %+v", result)
	}
	records, err := s.RouteRecords(app.ApplicationID)
	if err != nil || len(records) != 1 {
		t.Fatalf("route records: %v %+v", err, records)
	}
	record := records[0]
	if record.PolicyVersion != 1 || record.Provider != "echo" ||
		record.Status != "failed" || record.Reason != "route_denied" {
		t.Fatalf("route record wrong: %+v", record)
	}
}

func TestRunDeniesTurnOutsidePolicy(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetRoutePolicy("tester", app.ApplicationID, []string{"llama.cpp"}); err != nil {
		t.Fatal(err)
	}
	sent, err := s.Enqueue("tester", app.ApplicationID, "k1",
		json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	// The echo provider is not in the policy, so the runner must fail the
	// turn without ever handing the prompt to a backend.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := s.Run(ctx, EchoProvider{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	result, err := s.Result(app.ApplicationID, sent.MessageID)
	if err != nil || result.Status != "failed" {
		t.Fatalf("turn not denied: %+v %v", result, err)
	}
	records, _ := s.RouteRecords(app.ApplicationID)
	if len(records) != 1 || records[0].Reason != "route_denied" ||
		records[0].PolicyVersion != 1 {
		t.Fatalf("route record wrong: %+v", records)
	}
}

// Recovery must preserve the frozen failure cause: the prepared record
// carries it, so a crash between staging and the final persist cannot
// relabel a provider failure or a policy denial as a text overflow.
func TestRecoveryKeepsFrozenFailureReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
	}{
		{"route_denied"},
		{"provider_failed"},
		{"text_overflow"},
	} {
		s := newKernelTestSupervisor(t)
		app, err := s.CreateApplication("tester", "owner", "on_event")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, err := s.Enqueue("tester", app.ApplicationID, "k",
			json.RawMessage(`{"text":"hi"}`)); err != nil {
			t.Fatal(err)
		}
		id, m, err := s.claimTurn()
		if err != nil || id == "" {
			t.Fatalf("claim: %v", err)
		}
		// Simulate the post-crash retry: the first pass staged a failed
		// prepared record (with its cause) but never reached the final
		// result persist. The retry takes the prepared-recovery path.
		s.mu.Lock()
		s.state.Prepared[m.MessageID] = TurnResult{
			ApplicationID: id, MessageID: m.MessageID,
			Status: "failed", Text: "", Usage: Usage{InputTokens: 2, OutputTokens: 2},
			Provider: "echo", FailureReason: tc.reason,
		}
		m.Status = "claimed"
		s.state.Mailbox[id][0].Status = "claimed"
		s.mu.Unlock()
		if err := s.finishTurn(id, m, "", Usage{}, RouteInfo{Provider: "echo"}, nil); err != nil {
			t.Fatalf("recovery finishTurn: %v", err)
		}
		records, _ := s.RouteRecords(app.ApplicationID)
		if len(records) != 1 || records[0].Reason != tc.reason {
			t.Fatalf("recovery relabeled %s as %+v", tc.reason, records)
		}
	}
}
