package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// The second revocation level of the P3 contract: the kernel revokes the
// inference capability it issued (0006 revoke, probe-verified); this
// registry revokes runtime capability tokens. Revoking a token also revokes
// every descendant in its attenuation chain: attenuation re-signs with the
// same issuer key and only ever appends caveats, so an ancestor's caveat
// list is a strict prefix of each descendant's.
type RevokedToken struct {
	Digest    string   `json:"digest"`
	PublicKey []byte   `json:"public_key"`
	Subject   string   `json:"subject"`
	Caveats   []string `json:"caveats"`
	Time      int64    `json:"time"`
	Reason    string   `json:"reason,omitempty"`
}

const maxRevokedTokens = 1024

var ErrRevocationCapacity = errors.New("revocation registry is full")

func tokenDigest(t CapabilityToken) string {
	sum := sha256.Sum256(append(tokenBytes(t), t.Signature...))
	return hex.EncodeToString(sum[:])
}

// revokedBy reports whether the token is the revoked entry itself or a
// descendant of it: same issuer key, same subject, and the entry's caveats
// form a prefix of the token's caveats. Subject equality keeps
// independently attenuated sibling chains from the same issuer key (which
// share early caveat strings) from cross-revoking each other; a revoked
// root-tier entry (empty caveats) matches only tokens for that subject.
func (r RevokedToken) revokedBy(t CapabilityToken) bool {
	if r.Subject != t.Subject || !bytesEqual(r.PublicKey, t.PublicKey) {
		return false
	}
	// Exact-digest match is the revocation itself. Otherwise only a
	// STRICT caveat prefix counts: coincident siblings (same key, subject
	// and caveat list, independently attenuated) share no lineage edge,
	// so revoking one leaves the other intact.
	if tokenDigest(t) == r.Digest {
		return true
	}
	if len(r.Caveats) >= len(t.Caveats) {
		return false
	}
	for i, caveat := range r.Caveats {
		if t.Caveats[i] != caveat {
			return false
		}
	}
	return true
}

func (s *Supervisor) tokenRevokedLocked(t CapabilityToken) bool {
	for _, r := range s.state.RevokedTokens {
		if r.revokedBy(t) {
			return true
		}
	}
	return false
}

// RevokeCapabilityToken enters a token (and its whole attenuation chain)
// into the revocation registry. The token must still verify: revocation
// removes authority, it does not fabricate it.
func (s *Supervisor) RevokeCapabilityToken(principal string, token CapabilityToken, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := token.Verify(time.Now()); err != nil {
		return errors.New("revocation requires a verifiable token")
	}
	if len(reason) > 256 {
		return errors.New("invalid revocation reason")
	}
	digest := tokenDigest(token)
	for _, r := range s.state.RevokedTokens {
		if r.Digest == digest {
			return nil // idempotent
		}
	}
	// Effective revocations must never be evicted to admit a new entry.
	if len(s.state.RevokedTokens) >= maxRevokedTokens {
		_ = s.auditLocked(principal, "", "capability.revoke", digest, "deny", "registry full")
		if err := s.persistLocked(nil); err != nil {
			return err
		}
		return ErrRevocationCapacity
	}
	entry := RevokedToken{Digest: digest,
		PublicKey: append([]byte(nil), token.PublicKey...),
		Subject:   token.Subject,
		Caveats:   append([]string(nil), token.Caveats...),
		Time:      time.Now().UnixNano(), Reason: reason}
	s.state.RevokedTokens = append(s.state.RevokedTokens, entry)
	_ = s.auditLocked(principal, "", "capability.revoke", digest, "allow",
		strings.Join(append([]string(nil), token.Caveats...), "|"))
	return s.persistLocked(nil)
}
