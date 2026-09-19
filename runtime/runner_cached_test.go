package runtime

import (
	"encoding/json"
	"testing"
)

// The cached-prefix ledger is what makes the hit rate recomputable from
// application.inspect plus per-turn message.result payloads.
func TestFinishTurnAccountsCachedTokens(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if _, err := s.Enqueue("tester", app.ApplicationID, "k1",
		json.RawMessage(`{"text":"hello"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	id, m, err := s.claimTurn()
	if err != nil || id == "" {
		t.Fatalf("claim: %v %v", id, err)
	}
	usage := Usage{InputTokens: 100, OutputTokens: 10, CachedTokens: 60}
	if err := s.finishTurn(id, m, "done", usage, RouteInfo{Provider: "echo", CompatKey: "echo|std|plain|f32|1|cpu"}, nil); err != nil {
		t.Fatalf("finishTurn: %v", err)
	}
	inspected, err := s.InspectApplication(app.ApplicationID)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if inspected.TokensCached != 60 {
		t.Fatalf("cached tokens not accumulated: %d", inspected.TokensCached)
	}
	result, err := s.Result(app.ApplicationID, m.MessageID)
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if result.Usage.CachedTokens != 60 || result.Usage.InputTokens != 100 {
		t.Fatalf("per-turn cached usage missing: %+v", result.Usage)
	}
}
