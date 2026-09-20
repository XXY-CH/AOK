package runtime

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestRevocationCapacityPreservesExistingEntries(t *testing.T) {
	s := newKernelTestSupervisor(t)
	_, issuer, err := GenerateCapabilityKey()
	if err != nil {
		t.Fatal(err)
	}
	var first CapabilityToken
	for i := 0; i <= maxRevokedTokens; i++ {
		token, err := MintCapabilityToken(issuer, "supervisor", fmt.Sprintf("subject-%d", i),
			time.Now().Add(time.Hour), []CapabilityGrant{{Object: "net", Action: "*"}})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = token
		}
		err = s.RevokeCapabilityToken("tester", token, "test")
		if i == maxRevokedTokens {
			if !errors.Is(err, ErrRevocationCapacity) {
				t.Fatalf("overflow must be explicit: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RevokeCapabilityToken("tester", first, "idempotent at capacity"); err != nil {
		t.Fatal(err)
	}
	s.restoreLocked()
	if !s.tokenRevokedLocked(first) || len(s.state.RevokedTokens) != maxRevokedTokens {
		t.Fatal("capacity overflow lost a committed revocation")
	}
}

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

// Sibling chains from one issuer key must not cross-revoke: independent
// roots for different subjects, and branches sharing an early caveat
// string, stay usable when the other branch is revoked.
func TestRevocationSiblingsDoNotCrossRevoke(t *testing.T) {
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
	grant := []CapabilityGrant{{Object: "net", Action: "fetch"}}
	// Two independent roots for different subjects from the same key.
	alice, err := MintCapabilityToken(issuer, "supervisor", "alice", time.Now().Add(time.Hour), grant)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := MintCapabilityToken(issuer, "supervisor", "bob", time.Now().Add(time.Hour), grant)
	if err != nil {
		t.Fatal(err)
	}
	// Revoking alice's root-tier chain covers alice's subject only: bob
	// (same key, different subject) stays usable — alice's own chain is
	// expected to die with her root, which is the cascade working.
	if err := s.RevokeCapabilityToken("tester", alice, "compromise"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckWithToken("bob", app.ApplicationID, "net", "fetch", bob); err != nil {
		t.Fatalf("bob's independent root cross-revoked: %v", err)
	}

	// Coincident siblings: two branches of a NOT-yet-revoked root sharing
	// the same caveat string. Revoking one digest leaves the other and its
	// leaf intact (no lineage edge between coincident siblings).
	carol, err := MintCapabilityToken(issuer, "supervisor", "carol", time.Now().Add(time.Hour), grant)
	if err != nil {
		t.Fatal(err)
	}
	// Sibling branches share the first caveat string but diverge after it;
	// tokens with fully identical caveat lists are byte-identical authority
	// (same digest), so divergence is what makes them siblings.
	branchA, err := carol.Attenuate(issuer, "shared-first-caveat", grant)
	if err != nil {
		t.Fatal(err)
	}
	branchA, err = branchA.Attenuate(issuer, "path=a", grant)
	if err != nil {
		t.Fatal(err)
	}
	branchB, err := carol.Attenuate(issuer, "shared-first-caveat", grant)
	if err != nil {
		t.Fatal(err)
	}
	leafB, err := branchB.Attenuate(issuer, "domain=example.org", grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeCapabilityToken("tester", branchA, "rotate"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckWithToken("carol", app.ApplicationID, "net", "fetch", branchB); err != nil {
		t.Fatalf("coincident sibling branch cross-revoked: %v", err)
	}
	if _, err := s.CheckWithToken("carol", app.ApplicationID, "net", "fetch", leafB); err != nil {
		t.Fatalf("sibling leaf cross-revoked: %v", err)
	}
	if _, err := s.CheckWithToken("carol", app.ApplicationID, "net", "fetch", branchA); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("branchA survived its own revocation: %v", err)
	}
	// And revoking carol's root now takes her whole remaining chain.
	if err := s.RevokeCapabilityToken("tester", carol, "offboard"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckWithToken("carol", app.ApplicationID, "net", "fetch", leafB); !errors.Is(err, ErrCapabilityDenied) {
		t.Fatalf("root cascade did not reach the sibling leaf: %v", err)
	}
}
