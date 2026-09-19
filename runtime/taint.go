package runtime

import (
	"encoding/json"
	"errors"
)

// Taint labels dataflow through the supervisor boundary. P3 contract
// (PLAN-L0 3.5 / route-model): tainted data must not flow to outbound
// surfaces without an explicit grant. Bits are additive: message payloads
// declare them, an application accumulates them through every turn that
// consumed tainted input, and the export gate below is the only way out.
const (
	TaintExternal     uint64 = 1 << iota // data from outside the SubOS trust domain
	TaintConfidential                    // confidentiality-tagged context
)

var ErrTaintBlocked = errors.New("tainted data blocked at the export gate")

// taintedPayload reads the optional taint declaration of a message payload.
func taintedPayload(payload json.RawMessage) uint64 {
	var p struct {
		Taint uint64 `json:"taint"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.Taint
}

// exportApplication ships text out of the SubOS boundary. The application's
// accumulated taint must fit the manifest's export mask; tainted bits that
// are not masked reject the export unless the caller explicitly confirms,
// which downgrades the block into an audited slow-path approval (the
// confirmation itself is the auditable artifact).
func (s *Supervisor) exportApplication(principal, id, text string, confirm bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return "", ErrApplicationNotFound
	}
	if a.State == "tombstoned" || a.State == "retiring" {
		return "", ErrApplicationRetired
	}
	if text == "" || len(text) > maxTextBytes {
		return "", errors.New("invalid export payload")
	}
	blocked := a.TaintBits & ^s.policy.ExportMask
	if blocked == 0 {
		_ = s.auditLocked(principal, id, "taint.export", text, "allow", "clean")
		if err := s.persistLocked(nil); err != nil {
			return "", err
		}
		return text, nil
	}
	if !confirm {
		_ = s.auditLocked(principal, id, "taint.export", text, "deny", "unmasked taint")
		if err := s.persistLocked(nil); err != nil {
			return "", err
		}
		return "", ErrTaintBlocked
	}
	_ = s.auditLocked(principal, id, "taint.export.confirm", text, "allow",
		"human-confirmed unmasked taint")
	if err := s.persistLocked(nil); err != nil {
		return "", err
	}
	return text, nil
}

// accumulateTaint folds a message's declared taint into the application so
// later turns (and their outputs) inherit it. Caller holds s.mu.
func (s *Supervisor) accumulateTaint(id string, taint uint64) {
	if taint == 0 {
		return
	}
	if a, ok := s.state.Applications[id]; ok {
		a.TaintBits |= taint
	}
}

// foldTaint is the locked entry for provider-reported taint (kernel
// inference sessions label their results with the session policy taint).
func (s *Supervisor) foldTaint(id string, taint uint64) {
	if taint == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accumulateTaint(id, taint)
	_ = s.persistLocked(nil)
}
