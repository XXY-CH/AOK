package runtime

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strconv"
)

// Witness co-signing (Sigsum-shaped): the local audit chain is an unkeyed
// hash chain, so anyone who can rewrite the state can rebuild it
// consistently. A checkpoint co-signed by an independent witness key
// anchors the history externally: a recorded signature over (head hash,
// record count, time) cannot be re-forged after a rewrite without the
// witness key, and VerifyWitness detects any divergence.
type WitnessCheckpoint struct {
	HeadHash  string `json:"head_hash"`
	Count     uint64 `json:"count"`
	Time      int64  `json:"time"`
	PublicKey []byte `json:"public_key"`
	Signature []byte `json:"signature"`
	Signer    string `json:"signer,omitempty"`
}

const maxWitnessCheckpoints = 256

var ErrWitnessMismatch = errors.New("audit history diverges from a witness checkpoint")

// checkpointMessage is the canonical bytes a witness signs.
func checkpointMessage(cp WitnessCheckpoint) []byte {
	return []byte("aok-audit-checkpoint:v1|" + cp.HeadHash + "|" +
		strconv.FormatUint(cp.Count, 10) + "|" + strconv.FormatInt(cp.Time, 10))
}

// WitnessHead returns the current chain head for an external witness to
// sign: the newest audit record's hash and the record count.
func (s *Supervisor) WitnessHead() (head string, count uint64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := uint64(len(s.state.Audit))
	if n == 0 {
		return "", 0, errors.New("audit chain is empty")
	}
	return s.state.Audit[n-1].Hash, n, nil
}

// RecordCosignature accepts a witness signature over a checkpoint. The
// signature must verify against the enclosed key and the checkpoint must
// match the chain at its position; mismatching checkpoints are recorded
// nowhere — they are the attack this exists to catch, and VerifyWitness
// compares against what IS recorded.
func (s *Supervisor) RecordCosignature(principal string, cp WitnessCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(cp.PublicKey) != ed25519.PublicKeySize ||
		len(cp.Signature) != ed25519.SignatureSize || cp.Count == 0 ||
		cp.HeadHash == "" || cp.Time == 0 {
		return errors.New("invalid witness checkpoint")
	}
	if !ed25519.Verify(ed25519.PublicKey(cp.PublicKey), checkpointMessage(cp), cp.Signature) {
		return errors.New("witness signature mismatch")
	}
	if cp.Count > uint64(len(s.state.Audit)) ||
		s.state.Audit[cp.Count-1].Hash != cp.HeadHash {
		return ErrWitnessMismatch
	}
	if len(s.state.WitnessCheckpoints) > 0 {
		last := s.state.WitnessCheckpoints[len(s.state.WitnessCheckpoints)-1]
		if cp.Count <= last.Count {
			return errors.New("checkpoint does not advance the witnessed history")
		}
	}
	entry := WitnessCheckpoint{HeadHash: cp.HeadHash, Count: cp.Count, Time: cp.Time,
		PublicKey: append([]byte(nil), cp.PublicKey...),
		Signature: append([]byte(nil), cp.Signature...), Signer: cp.Signer}
	s.state.WitnessCheckpoints = append(s.state.WitnessCheckpoints, entry)
	if overflow := len(s.state.WitnessCheckpoints) - maxWitnessCheckpoints; overflow > 0 {
		s.state.WitnessCheckpoints = append([]WitnessCheckpoint(nil),
			s.state.WitnessCheckpoints[overflow:]...)
	}
	_ = s.auditLocked(principal, "", "witness.cosign",
		fmt.Sprintf("%s@%d", cp.HeadHash[:12], cp.Count), "allow", cp.Signer)
	return s.persistLocked(nil)
}

// VerifyWitness checks the audit chain against every recorded checkpoint:
// the local chain must still verify (VerifyAudit) and each checkpoint's
// head must equal the hash at its position. A rewritten history fails even
// when internally consistent.
func (s *Supervisor) VerifyWitness() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := VerifyAudit(s.state.Audit); err != nil {
		return err
	}
	for _, cp := range s.state.WitnessCheckpoints {
		if cp.Count > uint64(len(s.state.Audit)) ||
			s.state.Audit[cp.Count-1].Hash != cp.HeadHash {
			return fmt.Errorf("%w at record %d", ErrWitnessMismatch, cp.Count)
		}
		if !ed25519.Verify(ed25519.PublicKey(cp.PublicKey), checkpointMessage(cp), cp.Signature) {
			return errors.New("recorded witness signature invalid")
		}
	}
	return nil
}

func (s *Supervisor) WitnessCheckpoints() []WitnessCheckpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]WitnessCheckpoint(nil), s.state.WitnessCheckpoints...)
}
