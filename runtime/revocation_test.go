package runtime

import (
	"errors"
	"os"
	"testing"
	"time"
)

// The second revocation level: revoking a runtime capability token takes
// its whole attenuation chain with it, survives restarts, and leaves
// unrelated tokens from the same issuer untouched.
func TestRevocationCoversAttenuationChain(t *testing.T) {
	// The authorizer snapshots the policy at construction, so the manifest
	// must grant net up front.
	root0 := t.TempDir()
	if err := os.Chmod(root0, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewSupervisor(root0, CapabilitySet{Engine: "echo", Net: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	_, issuer, err := GenerateCapabilityKey()
	if err != nil {
		t.Fatal(err)
	}
	root, err := MintCapabilityToken(issuer, "supervisor", "tester",
		time.Now().Add(time.Hour), []CapabilityGrant{{Object: "net", Action: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	middle, err := root.Attenuate(issuer, "rate=10/s",
		[]CapabilityGrant{{Object: "net", Action: "fetch"}})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := middle.Attenuate(issuer, "domain=example.org",
		[]CapabilityGrant{{Object: "net", Action: "fetch"}})
	if err != nil {
		t.Fatal(err)
	}

	// All three verify and allow before revocation.
	for name, token := range map[string]CapabilityToken{"root": root, "middle": middle, "leaf": leaf} {
		ok, err := s.CheckWithToken("tester", app.ApplicationID, "net", "fetch", token)
		if err != nil || !ok {
			t.Fatalf("%s denied before revocation: %v", name, err)
		}
	}

	// Revoking the middle token kills the leaf (a descendant) but leaves
	// the root (an ancestor) usable for a fresh attenuation.
	if err := s.RevokeCapabilityToken("tester", middle, "compromised holder"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckWithToken("tester", app.ApplicationID, "net", "fetch", leaf); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("leaf survived ancestor revocation: %v", err)
	}
	if _, err := s.CheckWithToken("tester", app.ApplicationID, "net", "fetch", middle); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("revoked token still accepted: %v", err)
	}
	if ok, err := s.CheckWithToken("tester", app.ApplicationID, "net", "fetch", root); err != nil || !ok {
		t.Fatalf("root collateral damage: %v", err)
	}
	// A new descendant of the revoked middle is still-born: its caveats
	// extend the revoked prefix.
	reborn, err := middle.Attenuate(issuer, "retry", []CapabilityGrant{{Object: "net", Action: "fetch"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckWithToken("tester", app.ApplicationID, "net", "fetch", reborn); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("fresh descendant of revoked token accepted: %v", err)
	}

	// The registry is durable and idempotent.
	if err := s.RevokeCapabilityToken("tester", middle, "again"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root0, CapabilitySet{Engine: "echo", Net: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.CheckWithToken("tester", app.ApplicationID, "net", "fetch", leaf); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("revocation lost across restart: %v", err)
	}

	// Audit carries the revocation with its reason.
	found := false
	for _, r := range s2.Audit() {
		if r.Action == "capability.revoke" && r.Decision == "allow" {
			found = true
		}
	}
	if !found {
		t.Fatal("revocation not audited")
	}
}

func TestRevocationRequiresVerifiableToken(t *testing.T) {
	s := newKernelTestSupervisor(t)
	_, other, err := GenerateCapabilityKey()
	if err != nil {
		t.Fatal(err)
	}
	forged, err := MintCapabilityToken(other, "attacker", "victim",
		time.Now().Add(time.Hour), []CapabilityGrant{{Object: "net", Action: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	// Expire it: revocation must not register untrusted/unverifiable input.
	forged.ExpiresAt = time.Now().Add(-time.Hour).Unix()
	if err := s.RevokeCapabilityToken("tester", forged, "x"); err == nil {
		t.Fatal("expired token accepted for revocation")
	}
}
