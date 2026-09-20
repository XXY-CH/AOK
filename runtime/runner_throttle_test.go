package runtime

import (
	"encoding/json"
	"testing"
	"time"
)

func TestClaimTurnTokenThrottle(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	s.mu.Lock()
	a := s.state.Applications[app.ApplicationID]
	a.TokenLimit = 10
	a.TokensUsed = 8 // 80%: inside the throttle band
	s.mu.Unlock()
	for i := 0; i < 2; i++ {
		if _, err := s.Enqueue("tester", app.ApplicationID,
			"bulk-"+string(rune('a'+i)), json.RawMessage(`{"text":"x"}`)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	id, first, err := s.claimTurn()
	if err != nil || id != app.ApplicationID {
		t.Fatalf("first claim in throttle band must still run: %v %v", id, err)
	}
	// A turn settles before the application claims again: parallel fan-out
	// spreads across applications, never within one application.
	if err := s.finishTurn(id, first, "ok", Usage{},
		RouteInfo{Provider: "echo"}, nil); err != nil {
		t.Fatal(err)
	}
	if id, _, err = s.claimTurn(); err != nil || id != "" {
		t.Fatalf("immediate re-claim must be throttled: %v %v", id, err)
	}
	s.mu.Lock()
	s.lastClaim[app.ApplicationID] = time.Now().Add(-2 * throttleClaimInterval)
	s.mu.Unlock()
	id, second, err := s.claimTurn()
	if err != nil || id != app.ApplicationID || second.MessageID == first.MessageID {
		t.Fatalf("claim after the interval must resume: %v %v", id, err)
	}
	if err := s.finishTurn(id, second, "ok", Usage{},
		RouteInfo{Provider: "echo"}, nil); err != nil {
		t.Fatal(err)
	}

	// Below the band, admission stays immediate.
	s.mu.Lock()
	s.state.Applications[app.ApplicationID].TokensUsed = 0
	s.mu.Unlock()
	if _, err := s.Enqueue("tester", app.ApplicationID, "after",
		json.RawMessage(`{"text":"y"}`)); err != nil {
		t.Fatalf("enqueue after: %v", err)
	}
	if id, _, err = s.claimTurn(); err != nil || id != app.ApplicationID {
		t.Fatalf("claims below the band must not be throttled: %v %v", id, err)
	}
}
