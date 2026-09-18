package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type TurnResult struct {
	ApplicationID string `json:"application_id"`
	MessageID     string `json:"message_id"`
	Text          string `json:"text"`
	Usage         Usage  `json:"usage"`
	Status        string `json:"status"`
	Checkpoint    string `json:"checkpoint"`
}

type ApplicationTimer struct {
	ApplicationID string          `json:"application_id"`
	TimerID       string          `json:"timer_id"`
	Due           int64           `json:"due"`
	Interval      int64           `json:"interval"`
	Payload       json.RawMessage `json:"payload"`
}

func (s *Supervisor) SetApplicationState(principal, id, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return ErrApplicationNotFound
	}
	if a.State == "tombstoned" {
		return ErrApplicationRetired
	}
	if state != "frozen" && state != "serving" {
		return errors.New("invalid application state")
	}
	if state == "serving" && a.TokenLimit > 0 && a.TokensUsed >= a.TokenLimit {
		return errors.New("token budget exhausted")
	}
	a.State = state
	_ = s.auditLocked(principal, id, "application."+state, id, "allow", "")
	return s.persistLocked(nil)
}

func (s *Supervisor) SetTokenLimit(principal, id string, limit uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return ErrApplicationNotFound
	}
	if a.State == "tombstoned" {
		return ErrApplicationRetired
	}
	if a.TokenLimit != 0 && (limit == 0 || limit > a.TokenLimit) {
		return errors.New("budget may only narrow")
	}
	a.TokenLimit = limit
	if limit > 0 && a.TokensUsed >= limit {
		a.State = "frozen"
	}
	_ = s.auditLocked(principal, id, "budget.set", id, "allow", "")
	return s.persistLocked(nil)
}

func (s *Supervisor) AddTimer(principal, id, timerID string, delay, interval time.Duration, payload json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return ErrApplicationNotFound
	}
	if a.State == "tombstoned" {
		return ErrApplicationRetired
	}
	if timerID == "" || len(timerID) > 128 || delay <= 0 || interval < 0 || !json.Valid(payload) || len(payload) > maxTextBytes {
		return errors.New("invalid timer")
	}
	key := id + ":" + timerID
	if _, ok = s.state.Timers[key]; ok {
		return errors.New("timer already exists")
	}
	s.state.Timers[key] = ApplicationTimer{ApplicationID: id, TimerID: timerID, Due: time.Now().Add(delay).UnixNano(), Interval: int64(interval), Payload: append(json.RawMessage(nil), payload...)}
	_ = s.auditLocked(principal, id, "timer.create", timerID, "allow", "")
	return s.persistLocked(nil)
}

func (s *Supervisor) deliverTimers() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixNano()
	changed := false
	keys := make([]string, 0, len(s.state.Timers))
	for key := range s.state.Timers {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := s.state.Timers[keys[i]], s.state.Timers[keys[j]]
		if a.Due == b.Due {
			return keys[i] < keys[j]
		}
		return a.Due < b.Due
	})
	for _, key := range keys {
		timer := s.state.Timers[key]
		a := s.state.Applications[timer.ApplicationID]
		if a == nil || a.State == "tombstoned" {
			delete(s.state.Timers, key)
			changed = true
			continue
		}
		if timer.Due > now {
			continue
		}
		keyID := fmt.Sprintf("timer:%s:%d", timer.TimerID, timer.Due)
		if _, err := s.enqueueLocked("supervisor", timer.ApplicationID, keyID, timer.Payload); err != nil {
			if errors.Is(err, ErrMailboxFull) {
				continue
			}
			s.restoreLocked()
			return err
		}
		if timer.Interval == 0 {
			delete(s.state.Timers, key)
		} else {
			timer.Due = now + timer.Interval
			s.state.Timers[key] = timer
		}
		_ = s.auditLocked("supervisor", timer.ApplicationID, "timer.deliver", keyID, "allow", "")
		changed = true
	}
	if changed {
		return s.persistLocked(nil)
	}
	return nil
}

func (s *Supervisor) claimTurn() (string, MailboxMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.state.Applications))
	for id := range s.state.Applications {
		ids = append(ids, id)
	}
	// Least charged application goes first; token cost, not wall-clock latency,
	// advances its virtual time. This is user-space scheduling, not sched_ext.
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.state.Applications[ids[i]], s.state.Applications[ids[j]]
		if a.TokensUsed == b.TokensUsed {
			return a.CreatedAt < b.CreatedAt
		}
		return a.TokensUsed < b.TokensUsed
	})
	for _, id := range ids {
		a := s.state.Applications[id]
		if a.State != "serving" || a.WakePolicy == "manual" {
			continue
		}
		for i := range s.state.Mailbox[id] {
			m := &s.state.Mailbox[id][i]
			if m.Status != "pending" {
				continue
			}
			m.Status = "claimed"
			m.Attempts++
			copy := *m
			copy.Payload = append(json.RawMessage(nil), m.Payload...)
			_ = s.auditLocked("supervisor", id, "turn.claim", m.MessageID, "allow", "")
			if err := s.persistLocked(nil); err != nil {
				return "", MailboxMessage{}, err
			}
			return id, copy, nil
		}
	}
	return "", MailboxMessage{}, nil
}

func (s *Supervisor) finishTurn(id string, m MailboxMessage, text string, usage Usage, providerErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.state.Applications[id]
	if a == nil {
		return ErrApplicationNotFound
	}
	if _, ok := s.state.Results[m.MessageID]; ok {
		return nil
	}
	prepared, ok := s.state.Prepared[m.MessageID]
	if !ok {
		status := "completed"
		if providerErr != nil || len(text) > maxTextBytes {
			status = "failed"
			text = ""
		}
		prepared = TurnResult{ApplicationID: id, MessageID: m.MessageID, Text: text, Usage: usage, Status: status}
		s.state.Prepared[m.MessageID] = prepared
		if err := s.persistLocked(nil); err != nil {
			return err
		}
	}
	text, usage = prepared.Text, prepared.Usage
	if prepared.Status == "failed" {
		providerErr = errors.New("provider failed")
	} else {
		providerErr = nil
	}
	resultBytes, err := json.Marshal(struct {
		Request json.RawMessage `json:"request"`
		Text    string          `json:"text"`
	}{m.Payload, text})
	if err != nil {
		return err
	}
	if _, err = s.contexts.Append(a.OwnerAgent, a.ContextID, m.MessageID+":result", resultBytes); err != nil {
		return err
	}
	checkpoint, err := s.contexts.Checkpoint(a.OwnerAgent, a.ContextID, m.MessageID+":checkpoint")
	if err != nil {
		return err
	}
	a.TokensUsed = saturatingAdd(a.TokensUsed, saturatingAdd(usage.InputTokens, usage.OutputTokens))
	status := "completed"
	if providerErr != nil {
		status = "failed"
		text = ""
		a.Failures++
	} else {
		a.Failures = 0
	}
	if a.TokenLimit > 0 && a.TokensUsed >= a.TokenLimit {
		a.State = "frozen"
		status = "budget_exhausted"
	}
	if a.State == "tombstoned" {
		status = "retired"
		text = ""
	}
	if len(text) > maxTextBytes {
		status = "failed"
		text = ""
	}
	s.state.Results[m.MessageID] = TurnResult{ApplicationID: id, MessageID: m.MessageID, Text: text, Usage: usage, Status: status, Checkpoint: checkpoint}
	delete(s.state.Prepared, m.MessageID)
	for i := range s.state.Mailbox[id] {
		if s.state.Mailbox[id][i].MessageID == m.MessageID {
			s.state.Mailbox[id][i].Status = "acked"
		}
	}
	// The prepared result fixes nondeterministic model output before idempotent
	// context writes. Recovery finishes that result without running the model.
	a.Checkpoint = checkpoint
	_ = s.auditLocked("supervisor", id, "turn."+status, m.MessageID, "allow", "")
	return s.persistLocked(nil)
}

// requeueClaim returns an in-flight delivery to the durable mailbox when the
// runner is stopped before it can commit a terminal result. The message ID is
// the transaction identity, so a later attempt can safely resume a prepared
// result or execute the provider again.
func (s *Supervisor) requeueClaim(applicationID, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Mailbox[applicationID] {
		m := &s.state.Mailbox[applicationID][i]
		if m.MessageID != messageID {
			continue
		}
		if m.Status != "claimed" {
			return nil
		}
		m.Status = "pending"
		_ = s.auditLocked("supervisor", applicationID, "turn.requeue", messageID, "allow", "runner stopped before commit")
		return s.persistLocked(nil)
	}
	return errors.New("claimed message not found")
}

func (s *Supervisor) Result(id, messageID string) (TurnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.state.Results[messageID]
	if !ok || r.ApplicationID != id {
		return TurnResult{}, errors.New("result not found")
	}
	return r, nil
}

// Run processes durable messages without a connected control client. The
// provider is supervisor-owned; message data never selects a backend or program.
func (s *Supervisor) Run(ctx context.Context, provider Provider) error {
	if provider == nil {
		return errors.New("runner requires provider")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if err := s.deliverTimers(); err != nil {
			return err
		}
		if err := s.deliverLSFS(); err != nil {
			return err
		}
		id, m, err := s.claimTurn()
		if err != nil {
			return err
		}
		if id == "" {
			continue
		}
		var p struct {
			Text string `json:"text"`
		}
		var text string
		var usage Usage
		s.mu.Lock()
		_, prepared := s.state.Prepared[m.MessageID]
		s.mu.Unlock()
		if !prepared {
			if err = json.Unmarshal(m.Payload, &p); err == nil {
				turnCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				text, usage, err = provider.Complete(turnCtx, p.Text)
				cancel()
			}
		}
		if ctx.Err() != nil {
			if requeueErr := s.requeueClaim(id, m.MessageID); requeueErr != nil {
				return requeueErr
			}
			return nil
		} // Recovery requeues the unacknowledged claim.
		if err = s.finishTurn(id, m, text, usage, err); err != nil {
			if requeueErr := s.requeueClaim(id, m.MessageID); requeueErr != nil {
				return fmt.Errorf("%w (requeue failed: %v)", err, requeueErr)
			}
			return err
		}
	}
}
