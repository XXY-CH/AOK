package runtime

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

// CapabilityGrant is the small fact carried by a Biscuit-style token. A grant
// can only be narrowed by attenuation; it cannot add a new object/action.
type CapabilityGrant struct {
	Object string `json:"object"`
	Action string `json:"action"`
}

type CapabilityToken struct {
	Issuer    string            `json:"issuer"`
	Subject   string            `json:"subject"`
	ExpiresAt int64             `json:"expires_at"`
	PublicKey []byte            `json:"public_key"`
	Grants    []CapabilityGrant `json:"grants"`
	Caveats   []string          `json:"caveats,omitempty"`
	Signature []byte            `json:"signature"`
}

func MintCapabilityToken(private ed25519.PrivateKey, issuer, subject string, expiresAt time.Time, grants []CapabilityGrant) (CapabilityToken, error) {
	if len(private) != ed25519.PrivateKeySize || issuer == "" || subject == "" || expiresAt.IsZero() || len(grants) == 0 {
		return CapabilityToken{}, errors.New("invalid capability token claims")
	}
	t := CapabilityToken{Issuer: issuer, Subject: subject, ExpiresAt: expiresAt.Unix(), PublicKey: append([]byte(nil), private.Public().(ed25519.PublicKey)...), Grants: cloneGrants(grants)}
	return signToken(t, private)
}

func GenerateCapabilityKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	return public, private, err
}

func (t CapabilityToken) Verify(now time.Time) error {
	if len(t.PublicKey) != ed25519.PublicKeySize || len(t.Signature) != ed25519.SignatureSize || t.Issuer == "" || t.Subject == "" || t.ExpiresAt < now.Unix() {
		return errors.New("invalid or expired capability token")
	}
	if !ed25519.Verify(ed25519.PublicKey(t.PublicKey), tokenBytes(t), t.Signature) {
		return errors.New("capability token signature mismatch")
	}
	return nil
}

// Attenuate re-signs a token with a caveat and an intersection of its grants.
// The caller must possess the issuer key; a holder cannot self-escalate.
func (t CapabilityToken) Attenuate(private ed25519.PrivateKey, caveat string, grants []CapabilityGrant) (CapabilityToken, error) {
	if err := t.Verify(time.Now()); err != nil {
		return CapabilityToken{}, err
	}
	if len(private) != ed25519.PrivateKeySize || caveat == "" || len(grants) == 0 || !bytesEqual(private.Public().(ed25519.PublicKey), t.PublicKey) {
		return CapabilityToken{}, errors.New("invalid attenuation")
	}
	allowed := make([]CapabilityGrant, 0, len(grants))
	for _, candidate := range grants {
		for _, parent := range t.Grants {
			if grantContains(parent, candidate) {
				allowed = append(allowed, candidate)
				break
			}
		}
	}
	if len(allowed) == 0 {
		return CapabilityToken{}, errors.New("attenuation removes all grants")
	}
	t.Grants = cloneGrants(allowed)
	t.Caveats = append(append([]string(nil), t.Caveats...), caveat)
	return signToken(t, private)
}

func (t CapabilityToken) Allows(object, action string) bool {
	for _, grant := range t.Grants {
		if grant.Object == object && grantContains(grant, CapabilityGrant{Object: object, Action: action}) {
			return true
		}
	}
	return false
}

func signToken(t CapabilityToken, private ed25519.PrivateKey) (CapabilityToken, error) {
	t.Signature = ed25519.Sign(private, tokenBytes(t))
	return t, nil
}

func tokenBytes(t CapabilityToken) []byte {
	t.Signature = nil
	type unsignedCapabilityToken CapabilityToken
	b, _ := json.Marshal(unsignedCapabilityToken(t))
	return b
}

func cloneGrants(in []CapabilityGrant) []CapabilityGrant {
	return append([]CapabilityGrant(nil), in...)
}

func bytesEqual(a, b []byte) bool { return sha256.Sum256(a) == sha256.Sum256(b) }

func grantContains(parent, child CapabilityGrant) bool {
	if parent.Object != child.Object {
		return false
	}
	if parent.Action == child.Action || parent.Action == "*" {
		return true
	}
	if strings.HasSuffix(parent.Action, "/**") {
		base := strings.TrimSuffix(parent.Action, "/**")
		return filepath.IsAbs(child.Action) && filepath.Clean(child.Action) == child.Action && (child.Action == base || strings.HasPrefix(child.Action, base+"/"))
	}
	return false
}

func (t CapabilityToken) MarshalText() ([]byte, error) {
	type wireCapabilityToken CapabilityToken
	b, err := json.Marshal(wireCapabilityToken(t))
	if err != nil {
		return nil, err
	}
	return []byte(base64.RawURLEncoding.EncodeToString(b)), nil
}

func ParseCapabilityToken(encoded string) (CapabilityToken, error) {
	b, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return CapabilityToken{}, errors.New("invalid capability token encoding")
	}
	var token CapabilityToken
	if json.Unmarshal(b, &token) != nil {
		return CapabilityToken{}, errors.New("invalid capability token")
	}
	return token, nil
}

type CedarRule struct {
	Effect    string `json:"effect"`
	Principal string `json:"principal"`
	Object    string `json:"object"`
	Action    string `json:"action"`
}

type CedarPolicy struct{ Rules []CedarRule }

// Evaluate implements Cedar's deny-overrides decision for the AOK object
// vocabulary. Wildcards are explicit and only match one field.
func (p CedarPolicy) Evaluate(principal, object, action string) bool {
	allowed := false
	for _, rule := range p.Rules {
		if !match(rule.Principal, principal) || !match(rule.Object, object) || !matchAction(rule.Action, action) {
			continue
		}
		if strings.EqualFold(rule.Effect, "deny") {
			return false
		}
		if strings.EqualFold(rule.Effect, "allow") {
			allowed = true
		}
	}
	return allowed
}

func match(rule, value string) bool { return rule == "*" || rule == value }
func matchAction(rule, value string) bool {
	return rule == "*" || rule == value || grantContains(CapabilityGrant{Action: rule}, CapabilityGrant{Action: value})
}

type CapabilityAuthorizer struct{ Policy CedarPolicy }

func (a CapabilityAuthorizer) Evaluate(principal, object, action string, token *CapabilityToken) error {
	if !a.Policy.Evaluate(principal, object, action) {
		return ErrCapabilityDenied
	}
	if token != nil {
		if err := token.Verify(time.Now()); err != nil || token.Subject != principal || !token.Allows(object, action) {
			return ErrCapabilityDenied
		}
	}
	return nil
}
