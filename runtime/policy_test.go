package runtime

import (
	"errors"
	"testing"
	"time"
)

func TestCapabilityTokenAttenuationAndCedarDeny(t *testing.T) {
	public, private, err := GenerateCapabilityKey()
	if err != nil || len(public) == 0 {
		t.Fatal(err)
	}
	token, err := MintCapabilityToken(private, "aok", "worker", time.Now().Add(time.Hour), []CapabilityGrant{{Object: "tool", Action: "search"}, {Object: "fs.read", Action: "/data/in/**"}})
	if err != nil || token.Verify(time.Now()) != nil {
		t.Fatal(err)
	}
	narrow, err := token.Attenuate(private, "ticket:123", []CapabilityGrant{{Object: "tool", Action: "search"}})
	if err != nil || !narrow.Allows("tool", "search") || narrow.Allows("fs.read", "/data/in/x") {
		t.Fatalf("narrow token=%+v err=%v", narrow, err)
	}
	policy := CapabilityAuthorizer{Policy: CedarPolicy{Rules: []CedarRule{
		{Effect: "allow", Principal: "worker", Object: "tool", Action: "*"},
		{Effect: "deny", Principal: "worker", Object: "tool", Action: "exec"},
	}}}
	if err := policy.Evaluate("worker", "tool", "search", &narrow); err != nil {
		t.Fatal(err)
	}
	if err := policy.Evaluate("worker", "tool", "exec", &narrow); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("deny error=%v", err)
	}
}
