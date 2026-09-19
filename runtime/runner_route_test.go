package runtime

import (
	"encoding/json"
	"testing"
)

func TestCacheHitKindClassification(t *testing.T) {
	echo := "echo|std|plain|f32|1|cpu"
	cases := []struct {
		name       string
		usage      Usage
		last, cur  string
		want       string
	}{
		{"first turn", Usage{}, "", echo, "text_replay"},
		{"same key no hit", Usage{}, echo, echo, "prefix_replay"},
		{"same key hit", Usage{CachedTokens: 87}, echo, echo, "kv_exact"},
		{"changed key hit", Usage{CachedTokens: 5}, echo, "anthropic|m|messages", "kv_exact"},
		{"changed key miss", Usage{}, echo, "anthropic|m|messages", "text_replay"},
	}
	for _, tc := range cases {
		if got := cacheHitKind(tc.usage, tc.last, tc.cur); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestFinishTurnRecordsRoute(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, _ := s.CreateApplication("tester", "owner", "on_event")
	if _, err := s.Enqueue("tester", app.ApplicationID, "k1",
		json.RawMessage(`{"text":"hello route"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	id, m, _ := s.claimTurn()
	if id == "" {
		t.Fatal("claim failed")
	}
	route := RouteInfo{Provider: "router", Fallbacks: 1, CompatKey: "llama.cpp|x"}
	if err := s.finishTurn(id, m, "ok", Usage{InputTokens: 5, OutputTokens: 2}, route, nil); err != nil {
		t.Fatalf("finishTurn: %v", err)
	}
	result, _ := s.Result(app.ApplicationID, m.MessageID)
	if result.Provider != "router" || result.Fallbacks != 1 ||
		result.CacheHitKind != "text_replay" {
		t.Fatalf("route record missing: %+v", result)
	}
	inspected, _ := s.InspectApplication(app.ApplicationID)
	if inspected.LastProvider != "router" || inspected.LastCompat != "llama.cpp|x" {
		t.Fatalf("application route state missing: %+v", inspected)
	}
}

func TestClaimTurnPrefixAffinity(t *testing.T) {
	s := newKernelTestSupervisor(t)
	first, _ := s.CreateApplication("tester", "a", "on_event")
	other, _ := s.CreateApplication("tester", "b", "on_event")
	shared, _ := s.CreateApplication("tester", "c", "on_event")
	for _, app := range []struct {
		id   string
		key  string
		text string
	}{{first.ApplicationID, "s1", "shared prefix payload alpha"},
		{other.ApplicationID, "s2", "unrelated payload beta"},
		{shared.ApplicationID, "s3", "shared prefix payload gamma"}} {
		if _, err := s.Enqueue("tester", app.id, app.key,
			json.RawMessage(`{"text":"`+app.text+`"}`)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	s.mu.Lock()
	s.lastPrefix = promptPrefix(json.RawMessage(`{"text":"shared prefix payload gamma"}`))
	s.mu.Unlock()
	// All three are equally charged; affinity must pull the shared-prefix
	// application ahead of the creation-order favourite.
	id, m, err := s.claimTurn()
	if err != nil || id != shared.ApplicationID {
		t.Fatalf("affinity did not apply: id=%s err=%v", id, err)
	}
	_ = s.requeueClaim(id, m.MessageID)

	// Outside the fairness band affinity must not override token fairness.
	s.mu.Lock()
	s.state.Applications[shared.ApplicationID].TokensUsed = 10_000
	s.mu.Unlock()
	id, _, err = s.claimTurn()
	if err != nil || id != first.ApplicationID {
		t.Fatalf("affinity escaped the fairness band: id=%s err=%v", id, err)
	}
}
