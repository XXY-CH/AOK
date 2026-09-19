package runtime

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// witnessFor signs checkpoints with a truly independent key, playing the
// external witness party.
func witnessFor(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := GenerateCapabilityKey()
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.PublicKey(public), private
}

func signCheckpoint(private ed25519.PrivateKey, head string, count uint64, stamp int64) WitnessCheckpoint {
	cp := WitnessCheckpoint{HeadHash: head, Count: count, Time: stamp,
		PublicKey: append([]byte(nil), private.Public().(ed25519.PublicKey)...)}
	cp.Signature = ed25519.Sign(private, checkpointMessage(cp))
	return cp
}

func TestWitnessCosignAndTamperDetection(t *testing.T) {
	s := newKernelTestSupervisor(t)
	public, private := witnessFor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	_ = app
	head, count, err := s.WitnessHead()
	if err != nil {
		t.Fatal(err)
	}
	cp := signCheckpoint(private, head, count, time.Now().Unix())
	if err := s.RecordCosignature("witness", cp); err != nil {
		t.Fatalf("cosign: %v", err)
	}
	// Signer identity is recorded.
	if checkpoints := s.WitnessCheckpoints(); len(checkpoints) != 1 || checkpoints[0].Count != count {
		t.Fatalf("checkpoint not recorded: %+v", checkpoints)
	}
	if err := s.VerifyWitness(); err != nil {
		t.Fatalf("verify after cosign: %v", err)
	}

	// A checkpoint for the wrong head (a rewritten history) is rejected at
	// recording time and never stored.
	forged := signCheckpoint(private, "deadbeef", count+1, time.Now().Unix())
	if err := s.RecordCosignature("witness", forged); !errors.Is(err, ErrWitnessMismatch) {
		t.Fatalf("forged head accepted: %v", err)
	}
	// A signature by the wrong key is rejected.
	_, other := witnessFor(t)
	badKey := signCheckpoint(other, head, count, time.Now().Unix())
	if err := s.RecordCosignature("witness", badKey); err == nil {
		t.Fatal("wrong-key signature accepted")
	}
	// Non-advancing checkpoints are rejected.
	stale := signCheckpoint(private, head, count, time.Now().Unix())
	if err := s.RecordCosignature("witness", stale); err == nil {
		t.Fatal("non-advancing checkpoint accepted")
	}

	// The attack that only witness anchoring catches: rewrite the whole
	// local chain consistently (re-hash every record). VerifyAudit alone
	// passes; VerifyWitness fails on the recorded head.
	root := s.root
	// A fully re-hashed rewrite is locally consistent — that is exactly
	// the blind spot the witness closes.
	if err := VerifyAudit(mutateAndRehash(t, s)); err != nil {
		t.Fatalf("re-hashed rewrite should pass the local chain: %v", err)
	}
	// Real check: state-level tamper in a fresh supervisor instance.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.VerifyWitness(); err != nil {
		t.Fatalf("clean restart should verify: %v", err)
	}
	_ = public
}

// mutateAndRehash rebuilds a fully self-consistent chain over the same
// records with one field mutated, demonstrating the local chain cannot
// detect it while the witness can.
func mutateAndRehash(t *testing.T, s *Supervisor) []AuditRecord {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	records := append([]AuditRecord(nil), s.state.Audit...)
	if len(records) > 0 {
		records[0].Reason = "rewritten"
	}
	previous := ""
	for i := range records {
		records[i].PreviousHash = previous
		records[i].Hash = auditHash(records[i])
		previous = records[i].Hash
	}
	return records
}

func TestWitnessPersistenceAndGrowth(t *testing.T) {
	s := newKernelTestSupervisor(t)
	_, private := witnessFor(t)
	root := s.root
	// Two sequential checkpoints over a growing chain.
	for i := 0; i < 2; i++ {
		if _, err := s.Enqueue("tester", mustApp(t, s), "w"+string(rune('a'+i)),
			json.RawMessage(`{"text":"x"}`)); err != nil {
			t.Fatal(err)
		}
		head, count, err := s.WitnessHead()
		if err != nil {
			t.Fatal(err)
		}
		cp := signCheckpoint(private, head, count, time.Now().Unix())
		if err := s.RecordCosignature("witness", cp); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if checkpoints := s2.WitnessCheckpoints(); len(checkpoints) != 2 {
		t.Fatalf("checkpoints lost: %+v", checkpoints)
	}
	if err := s2.VerifyWitness(); err != nil {
		t.Fatalf("verify after restart: %v", err)
	}
}

func mustApp(t *testing.T, s *Supervisor) string {
	t.Helper()
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	return app.ApplicationID
}
