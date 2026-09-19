package runtime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
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

// ErrConfirmationPending marks an export that was escalated to the human
// confirmation slow path instead of answered synchronously.
var ErrConfirmationPending = errors.New("export pending human confirmation")

// ConfirmationRequest is one pending human-approval item (the runtime side
// of the unotify slow path): a gate that would deny escalates here, and an
// external approver settles it within the TTL. Requests are one-shot.
type ConfirmationRequest struct {
	RequestID     string `json:"request_id"`
	ApplicationID string `json:"application_id"`
	Kind          string `json:"kind"`
	Object        string `json:"object"`
	Reason        string `json:"reason"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at"`
	Status        string `json:"status"` // pending | approved | denied | consumed | expired
	SettledBy     string `json:"settled_by,omitempty"`
}

const confirmationTTL = 5 * time.Minute

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
// exportApplication is the outbound gate with three settlement paths:
// clean exports pass; unmasked taint denies synchronously, escalates to a
// pending confirmation, or is released one-shot by an approved request.
func (s *Supervisor) exportApplication(principal, id, text string, confirm bool, escalate bool, requestID string) (string, error) {
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
	if requestID != "" {
		return s.exportViaRequestLocked(principal, a, text, requestID)
	}
	blocked := a.TaintBits & ^s.policy.ExportMask
	if blocked == 0 {
		_ = s.auditLocked(principal, id, "taint.export", text, "allow", "clean")
		if err := s.persistLocked(nil); err != nil {
			return "", err
		}
		return text, nil
	}
	if escalate && !confirm {
		return s.escalateExportLocked(principal, a, text)
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

func (s *Supervisor) escalateExportLocked(principal string, a *Application, text string) (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	now := time.Now()
	request := ConfirmationRequest{
		RequestID:     "cfm-" + hex.EncodeToString(raw[:]),
		ApplicationID: a.ApplicationID,
		Kind:          "taint.export", Object: text,
		Reason:    "unmasked taint requires human approval",
		CreatedAt: now.UnixNano(), ExpiresAt: now.Add(confirmationTTL).UnixNano(),
		Status: "pending",
	}
	s.state.Confirmations[request.RequestID] = request
	_ = s.auditLocked(principal, a.ApplicationID, "confirmation.create",
		request.RequestID, "allow", request.Kind)
	if err := s.persistLocked(nil); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%w: request_id=%s", ErrConfirmationPending, request.RequestID)
}

func (s *Supervisor) exportViaRequestLocked(principal string, a *Application, text, requestID string) (string, error) {
	request, ok := s.state.Confirmations[requestID]
	if !ok || request.ApplicationID != a.ApplicationID || request.Kind != "taint.export" {
		return "", errors.New("confirmation request not found")
	}
	switch {
	case request.Status == "consumed":
		return "", errors.New("confirmation request already used")
	case request.Status != "approved":
		return "", errors.New("confirmation request not approved")
	case time.Now().UnixNano() > request.ExpiresAt:
		request.Status = "expired"
		s.state.Confirmations[requestID] = request
		_ = s.persistLocked(nil)
		return "", errors.New("confirmation request expired")
	}
	request.Status = "consumed"
	s.state.Confirmations[requestID] = request
	_ = s.auditLocked(principal, a.ApplicationID, "taint.export.confirm",
		text, "allow", "one-shot via "+requestID)
	if err := s.persistLocked(nil); err != nil {
		return "", err
	}
	return text, nil
}

// SettleConfirmation approves or denies a pending request; the settler is
// audited and the decision is final (no re-settling).
func (s *Supervisor) SettleConfirmation(principal, requestID string, approve bool) (ConfirmationRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	request, ok := s.state.Confirmations[requestID]
	if !ok {
		return ConfirmationRequest{}, errors.New("confirmation request not found")
	}
	if request.Status != "pending" {
		return ConfirmationRequest{}, errors.New("confirmation request already settled")
	}
	if time.Now().UnixNano() > request.ExpiresAt {
		request.Status = "expired"
		s.state.Confirmations[requestID] = request
		_ = s.persistLocked(nil)
		return ConfirmationRequest{}, errors.New("confirmation request expired")
	}
	if approve {
		request.Status = "approved"
	} else {
		request.Status = "denied"
	}
	request.SettledBy = principal
	s.state.Confirmations[requestID] = request
	decision := "confirmation.deny"
	if approve {
		decision = "confirmation.approve"
	}
	_ = s.auditLocked(principal, request.ApplicationID, decision, requestID, "allow", request.Kind)
	return request, s.persistLocked(nil)
}

func (s *Supervisor) ListConfirmations(applicationID string) []ConfirmationRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ConfirmationRequest{}
	for _, request := range s.state.Confirmations {
		if applicationID == "" || request.ApplicationID == applicationID {
			out = append(out, request)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
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
