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
	Provider      string `json:"provider,omitempty"`
	Fallbacks     uint32 `json:"fallbacks,omitempty"`
	CacheHitKind  string `json:"cache_hit_kind,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
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

// throttleClaimInterval is the reduced admission rate for applications in
// the token pressure band (>=80% of the limit): turns still run, one per
// interval, instead of freezing at the first sign of pressure. The kernel
// reports the band as AOK_RES_LEVEL_THROTTLE; the freeze at the hard limit
// keeps its existing fail-closed behavior.
const throttleClaimInterval = time.Second

func (s *Supervisor) claimTurn() (string, MailboxMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
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
	chosen := ""
	var chosenMessage *MailboxMessage
	minTokens := ^uint64(0)
	type candidate struct {
		id      string
		message *MailboxMessage
		affine  bool
	}
	var candidates []candidate
	for _, id := range ids {
		a := s.state.Applications[id]
		if a.State != "serving" || a.WakePolicy == "manual" {
			continue
		}
		// Admission throttle: in the pressure band admit at most one
		// turn per interval; freezing only happens at the hard limit.
		if a.TokenLimit > 0 && a.TokensUsed < a.TokenLimit &&
			a.TokensUsed >= a.TokenLimit-a.TokenLimit/5 &&
			now.Sub(s.lastClaim[id]) < throttleClaimInterval {
			continue
		}
		if a.TokensUsed < minTokens {
			minTokens = a.TokensUsed
		}
		for i := range s.state.Mailbox[id] {
			m := &s.state.Mailbox[id][i]
			if m.Status != "pending" {
				continue
			}
			candidates = append(candidates, candidate{id: id, message: m,
				affine: s.lastPrefix != "" && promptPrefix(m.Payload) == s.lastPrefix})
			break
		}
	}
	for _, c := range candidates {
		// Prefix affinity: among candidates within the fairness band of
		// the least-charged application, prefer one sharing the last
		// executed prefix so the backend KV stays warm. The band keeps
		// token fairness intact and bounds starvation.
		if c.affine && s.state.Applications[c.id].TokensUsed <=
			saturatingAdd(minTokens, maxThrottleBand(minTokens)) {
			chosen, chosenMessage = c.id, c.message
			break
		}
		if chosen == "" {
			chosen, chosenMessage = c.id, c.message
		}
	}
	if chosen == "" {
		return "", MailboxMessage{}, nil
	}
	// Dataflow taint: a turn that consumes tainted input taints the
	// application context from here on, so its outputs inherit the label.
	s.accumulateTaint(chosen, taintedPayload(chosenMessage.Payload))
	chosenMessage.Status = "claimed"
	chosenMessage.Attempts++
	copy := *chosenMessage
	copy.Payload = append(json.RawMessage(nil), chosenMessage.Payload...)
	_ = s.auditLocked("supervisor", chosen, "turn.claim", chosenMessage.MessageID, "allow", "")
	if err := s.persistLocked(nil); err != nil {
		return "", MailboxMessage{}, err
	}
	s.lastClaim[chosen] = now
	return chosen, copy, nil
}

// maxThrottleBand bounds how far prefix affinity may jump past the
// least-charged candidate: never more than 25% or 64 tokens.
func maxThrottleBand(minTokens uint64) uint64 {
	band := minTokens / 4
	if band < 64 {
		band = 64
	}
	return band
}

// promptPrefix extracts the stable leading text a turn shares with fan-out
// siblings; it feeds both affinity scheduling and cache-hit accounting.
func promptPrefix(payload json.RawMessage) string {
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(payload, &p)
	const prefixLen = 64
	if len(p.Text) <= prefixLen {
		return p.Text
	}
	return p.Text[:prefixLen]
}

func (s *Supervisor) finishTurn(id string, m MailboxMessage, text string, usage Usage, route RouteInfo, providerErr error) error {
	// Capture the sentinel before the prepared-result path normalizes the
	// provider error into a generic failure.
	routeDenied := providerErr != nil && errors.Is(providerErr, ErrRouteDenied)
	overflowed := false
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.state.Applications[id]
	if a == nil {
		return ErrApplicationNotFound
	}
	if _, ok := s.state.Results[m.MessageID]; ok {
		return nil
	}
	cacheHit := cacheHitKind(usage, a.LastCompat, route.CompatKey)
	overflowed = providerErr == nil && len(text) > maxTextBytes
	prepared, ok := s.state.Prepared[m.MessageID]
	if !ok {
		status := "completed"
		frozen := ""
		if providerErr != nil {
			status = "failed"
			frozen = "provider_failed"
			if routeDenied {
				frozen = "route_denied"
			}
			text = ""
		} else if overflowed {
			status = "failed"
			frozen = "text_overflow"
			text = ""
		}
		prepared = TurnResult{ApplicationID: id, MessageID: m.MessageID, Text: text, Usage: usage, Status: status,
			Provider: route.Provider, Fallbacks: route.Fallbacks, CacheHitKind: cacheHit,
			FailureReason: frozen}
		s.state.Prepared[m.MessageID] = prepared
		if err := s.persistLocked(nil); err != nil {
			return err
		}
	}
	text, usage = prepared.Text, prepared.Usage
	if prepared.FailureReason == "text_overflow" {
		overflowed = true
	}
	if prepared.Provider != "" {
		route.Provider = prepared.Provider
		route.Fallbacks = prepared.Fallbacks
	}
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
	a.TokensCached = saturatingAdd(a.TokensCached, usage.CachedTokens)
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
		overflowed = true
	}
	s.state.Results[m.MessageID] = TurnResult{ApplicationID: id, MessageID: m.MessageID, Text: text, Usage: usage, Status: status, Checkpoint: checkpoint,
		Provider: route.Provider, Fallbacks: route.Fallbacks, CacheHitKind: cacheHit}
	delete(s.state.Prepared, m.MessageID)
	for i := range s.state.Mailbox[id] {
		if s.state.Mailbox[id][i].MessageID == m.MessageID {
			s.state.Mailbox[id][i].Status = "acked"
		}
	}
	// The prepared result fixes nondeterministic model output before idempotent
	// context writes. Recovery finishes that result without running the model.
	a.Checkpoint = checkpoint
	// The frozen reason wins on recovery: the prepared normalization
	// synthesizes a generic provider error, which would otherwise relabel
	// the original cause.
	reason := prepared.FailureReason
	if providerErr != nil && reason == "" {
		reason = "provider_failed"
	}
	if routeDenied {
		reason = "route_denied"
	}
	if overflowed {
		reason = "text_overflow"
	}
	s.recordRouteLocked(RouteRecord{
		ApplicationID: id, MessageID: m.MessageID,
		PolicyVersion: a.RoutePolicy.Version, Provider: route.Provider,
		CompatKey: route.CompatKey, Fallbacks: route.Fallbacks,
		CacheHitKind: cacheHit, PromptTokens: usage.InputTokens,
		CachedTokens: usage.CachedTokens, OutputTokens: usage.OutputTokens,
		Status: status, Reason: reason})
	if status != "failed" {
		a.LastProvider = route.Provider
		a.LastCompat = route.CompatKey
		s.lastPrefix = promptPrefix(m.Payload)
	}
	_ = s.auditLocked("supervisor", id, "turn."+status, m.MessageID, "allow", "")
	return s.persistLocked(nil)
}

// ErrRouteDenied marks a turn rejected by the application's route policy
// before any backend was contacted.
var ErrRouteDenied = errors.New("route denied by application policy")

// cacheHitKind classifies one turn against the route model's recovery
// levels: a backend-reported cache hit is kv_exact; the same compatibility
// key without a hit re-evaluates the prefix; a changed key forces
// text-level replay.
func cacheHitKind(usage Usage, lastCompat, currentCompat string) string {
	switch {
	case usage.CachedTokens > 0:
		return "kv_exact"
	case lastCompat != "" && lastCompat == currentCompat:
		return "prefix_replay"
	default:
		return "text_replay"
	}
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
	if err := s.attachKernel(); err != nil {
		return err
	}
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
		if err := s.deliverKernel(); err != nil {
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
		route := RouteInfo{Provider: provider.Name(), CompatKey: compatKeyOf(provider)}
		if !prepared {
			s.mu.Lock()
			policy := s.state.Applications[id].RoutePolicy
			s.mu.Unlock()
			// Self-enforcing providers (routers) read the policy from the
			// context; direct providers are checked here.
			if len(policy.Backends) > 0 {
				if _, selfEnforcing := provider.(interface{ LastRoute() RouteInfo }); !selfEnforcing &&
					!containsString(policy.Backends, provider.Name()) {
					err = ErrRouteDenied
				}
			}
			if err == nil {
				if err = json.Unmarshal(m.Payload, &p); err == nil {
					turnCtx, cancel := context.WithTimeout(WithRouteBackends(ctx, policy.Backends), 2*time.Minute)
					text, usage, err = provider.Complete(turnCtx, p.Text)
					cancel()
				}
			}
			if tracker, ok := provider.(interface{ LastRoute() RouteInfo }); ok && err == nil {
				route = tracker.LastRoute()
			}
		}
		if ctx.Err() != nil {
			if requeueErr := s.requeueClaim(id, m.MessageID); requeueErr != nil {
				return requeueErr
			}
			return nil
		} // Recovery requeues the unacknowledged claim.
		// Kernel inference reports the session taint with each result;
		// fold it into the application's dataflow ledger so the export
		// gate sees kernel-side labels too.
		if err == nil && !prepared {
			if tainted, ok := provider.(interface{ LastTaint() uint64 }); ok {
				s.foldTaint(id, tainted.LastTaint())
			}
		}
		if err = s.finishTurn(id, m, text, usage, route, err); err != nil {
			if requeueErr := s.requeueClaim(id, m.MessageID); requeueErr != nil {
				return fmt.Errorf("%w (requeue failed: %v)", err, requeueErr)
			}
			return err
		}
	}
}
