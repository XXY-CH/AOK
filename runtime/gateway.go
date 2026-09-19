package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// The message gateway: persistent channels bind external principals to
// applications (lsfs-schema.md message_* tables, runtime-snapshot form).
// Inbound messages enter the durable mailbox with a gateway idempotency
// key (unique per conversation and sequence) and wake the application per
// its policy; outbound replies land in a durable outbox and leave through
// an at-least-once claim/ack delivery protocol whose idempotency key makes
// reconnects deduplicate.
type MessageChannel struct {
	ChannelID string `json:"channel_id"`
	Kind      string `json:"kind"`  // e.g. webhook, im, email
	State     string `json:"state"` // active | paused | revoked
	CreatedAt int64  `json:"created_at"`
}

type MessageConversation struct {
	ChannelID     string `json:"channel_id"`
	ExternalID    string `json:"external_id"`
	Principal     string `json:"external_principal,omitempty"`
	ApplicationID string `json:"application_id"`
	LastSequence  int64  `json:"last_sequence"`
	CreatedAt     int64  `json:"created_at"`
}

type OutboxEntry struct {
	EntryID        string          `json:"entry_id"`
	ConversationID string          `json:"conversation_id"` // channel:external
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload"`
	Status         string          `json:"status"` // pending | sent
	Attempts       uint32          `json:"attempts"`
	CreatedAt      int64           `json:"created_at"`
	SentAt         int64           `json:"sent_at,omitempty"`
	Receipt        string          `json:"receipt,omitempty"`
}

// sourcedConversation records which conversation delivered the mailbox
// message a result answers, so replies route to the origin, never to an
// arbitrary conversation that happens to share the application.
var sourcedConversation sync.Map // messageID -> conversation key

var (
	ErrChannelNotFound      = errors.New("message channel not found")
	ErrConversationNotFound = errors.New("message conversation not found")
	ErrConversationBound    = errors.New("external identity already bound to another application")
)

const maxOutboxEntries = 256

func conversationKey(channelID, externalID string) string {
	return channelID + ":" + externalID
}

// CreateChannel registers an outbound-capable channel of the given kind.
func (s *Supervisor) CreateChannel(principal, channelID, kind string) (MessageChannel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if channelID == "" || len(channelID) > 128 || kind == "" || len(kind) > 64 {
		return MessageChannel{}, errors.New("invalid message channel")
	}
	if _, exists := s.state.Channels[channelID]; exists {
		return MessageChannel{}, errors.New("channel already exists")
	}
	channel := MessageChannel{ChannelID: channelID, Kind: kind,
		State: "active", CreatedAt: time.Now().UnixNano()}
	s.state.Channels[channelID] = channel
	_ = s.auditLocked(principal, "", "channel.create", channelID, "allow", kind)
	return channel, s.persistLocked(nil)
}

// RevokeChannel stops new deliveries on a channel; existing conversations
// keep their history but inbound is refused.
func (s *Supervisor) RevokeChannel(principal, channelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	channel, ok := s.state.Channels[channelID]
	if !ok {
		return ErrChannelNotFound
	}
	channel.State = "revoked"
	s.state.Channels[channelID] = channel
	_ = s.auditLocked(principal, "", "channel.revoke", channelID, "allow", "")
	return s.persistLocked(nil)
}

// BindConversation maps an external identity on a channel to an
// application. Binding is stable: the same identity cannot be remapped to
// a different application while active.
func (s *Supervisor) BindConversation(principal, channelID, externalID, applicationID string) (MessageConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	channel, ok := s.state.Channels[channelID]
	if !ok {
		return MessageConversation{}, ErrChannelNotFound
	}
	if channel.State != "active" {
		return MessageConversation{}, ErrChannelNotFound
	}
	if _, ok := s.state.Applications[applicationID]; !ok {
		return MessageConversation{}, ErrApplicationNotFound
	}
	if externalID == "" || len(externalID) > 256 {
		return MessageConversation{}, errors.New("invalid external identity")
	}
	key := conversationKey(channelID, externalID)
	if existing, ok := s.state.Conversations[key]; ok &&
		existing.ApplicationID != applicationID {
		return MessageConversation{}, ErrConversationBound
	}
	if existing, ok := s.state.Conversations[key]; ok {
		// Re-binding the same identity to the same application preserves
		// the cursor and creation time; wiping them would make already
		// routed sequences deliverable again.
		return existing, s.persistLocked(nil)
	}
	conversation := MessageConversation{ChannelID: channelID, ExternalID: externalID,
		ApplicationID: applicationID, CreatedAt: time.Now().UnixNano()}
	s.state.Conversations[key] = conversation
	_ = s.auditLocked(principal, applicationID, "conversation.bind", key, "allow", "")
	return conversation, s.persistLocked(nil)
}

// DeliverInbound routes one external message into the bound application's
// durable mailbox. The idempotency key is (channel, external id, sequence):
// a redelivery after a dropped connection enqueues exactly once, and the
// conversation cursor only advances when the mailbox accepted the event.
func (s *Supervisor) DeliverInbound(channelID, externalID string, sequence int64, payload json.RawMessage) (MailboxMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	channel, ok := s.state.Channels[channelID]
	if !ok || channel.State != "active" {
		return MailboxMessage{}, ErrChannelNotFound
	}
	key := conversationKey(channelID, externalID)
	conversation, ok := s.state.Conversations[key]
	if !ok {
		return MailboxMessage{}, ErrConversationNotFound
	}
	if sequence <= 0 || len(payload) > maxTextBytes || !json.Valid(payload) {
		return MailboxMessage{}, errors.New("invalid gateway message")
	}
	if sequence <= conversation.LastSequence {
		// Already routed: report the earlier delivery instead of duping.
		idempotency := fmt.Sprintf("gw:%s:%s:%d", channelID, externalID, sequence)
		for _, m := range s.state.Mailbox[conversation.ApplicationID] {
			if m.IdempotencyKey == idempotency {
				return m, nil
			}
		}
		return MailboxMessage{}, errors.New("sequence already routed but record missing")
	}
	// The envelope keeps the runner's flat {"text": ...} contract (the
	// model must see the body) and always declares external taint: the
	// gateway is outside the trust domain regardless of what the sender
	// claims.
	// Flat text contract: extract the body's text field so the model sees
	// exactly what the sender wrote (nested payload kept for provenance).
	var inner struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(payload, &inner)
	wrapped, err := json.Marshal(struct {
		Text      string          `json:"text"`
		Taint     uint64          `json:"taint"`
		Source    string          `json:"source"`
		Channel   string          `json:"channel"`
		Principal string          `json:"external_id"`
		Sequence  int64           `json:"sequence"`
		Payload   json.RawMessage `json:"payload"`
	}{inner.Text, TaintExternal, "gateway", channelID, externalID, sequence, payload})
	if err != nil {
		return MailboxMessage{}, err
	}
	idempotency := fmt.Sprintf("gw:%s:%s:%d", channelID, externalID, sequence)
	message, err := s.enqueueLocked("gateway", conversation.ApplicationID, idempotency, wrapped)
	if err != nil {
		return MailboxMessage{}, err
	}
	sourcedConversation.Store(message.MessageID, key)
	// Cursor advances only with the mailbox accept: a mail-rollback keeps
	// the old cursor, so the retry re-routes the same sequence idempotently.
	conversation.LastSequence = sequence
	s.state.Conversations[key] = conversation
	if err := s.persistLocked(nil); err != nil {
		return MailboxMessage{}, err
	}
	return message, nil
}

// enqueueOutboxLocked stages a reply for the outbound side. Caller holds s.mu.
func (s *Supervisor) enqueueOutboxLocked(conversationKey, idempotency string, payload json.RawMessage) (OutboxEntry, error) {
	conversation, ok := s.state.Conversations[conversationKey]
	if !ok {
		return OutboxEntry{}, ErrConversationNotFound
	}
	for _, entry := range s.state.Outbox {
		if entry.IdempotencyKey == idempotency {
			return entry, nil // at-least-once with dedup
		}
	}
	s.state.NextSequence++
	entry := OutboxEntry{
		EntryID:        fmt.Sprintf("out-%d", s.state.NextSequence),
		ConversationID: conversationKey,
		IdempotencyKey: idempotency,
		Payload:        append(json.RawMessage(nil), payload...),
		Status:         "pending", CreatedAt: time.Now().UnixNano(),
	}
	s.state.Outbox = append(s.state.Outbox, entry)
	if overflow := len(s.state.Outbox) - maxOutboxEntries; overflow > 0 {
		// Drop the oldest terminal entries first; pending work is kept.
		kept := s.state.Outbox[:0]
		dropped := 0
		for _, candidate := range s.state.Outbox {
			if dropped < overflow && candidate.Status != "pending" {
				dropped++
				continue
			}
			kept = append(kept, candidate)
		}
		s.state.Outbox = kept
	}
	_ = conversation
	return entry, nil
}

// Reply posts a turn result (or any application output) back to the
// conversation that sourced it, carrying the receipt (checkpoint/result id)
// the caller can verify against.
func (s *Supervisor) Reply(principal, applicationID, messageID string) (OutboxEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.state.Results[messageID]
	if !ok || result.ApplicationID != applicationID {
		return OutboxEntry{}, errors.New("result not found")
	}
	// Replies route to the conversation that sourced the message; with
	// several conversations bound to one application an arbitrary pick
	// would leak one principal's output to another.
	conversationKeyAny, ok := sourcedConversation.Load(messageID)
	conversationKey, _ := conversationKeyAny.(string)
	if !ok || conversationKey == "" {
		return OutboxEntry{}, ErrConversationNotFound
	}
	if _, ok := s.state.Conversations[conversationKey]; !ok {
		return OutboxEntry{}, ErrConversationNotFound
	}
	payload, err := json.Marshal(struct {
		Kind   string     `json:"kind"`
		Result TurnResult `json:"result"`
	}{"reply", result})
	if err != nil {
		return OutboxEntry{}, err
	}
	entry, err := s.enqueueOutboxLocked(conversationKey,
		"reply:"+messageID, payload)
	if err != nil {
		return OutboxEntry{}, err
	}
	_ = s.auditLocked(principal, applicationID, "gateway.reply",
		entry.EntryID, "allow", conversationKey)
	return entry, s.persistLocked(nil)
}

// ClaimOutbox hands the pending entries of one conversation to a
// transporter, marking them claimed-by-attempt. Reconnects re-claim;
// the idempotency key lets the remote side deduplicate.
func (s *Supervisor) ClaimOutbox(channelID, externalID string) ([]OutboxEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := conversationKey(channelID, externalID)
	if _, ok := s.state.Channels[channelID]; !ok {
		return nil, ErrChannelNotFound
	}
	if _, ok := s.state.Conversations[key]; !ok {
		return nil, ErrConversationNotFound
	}
	var out []OutboxEntry
	for i := range s.state.Outbox {
		if s.state.Outbox[i].ConversationID == key && s.state.Outbox[i].Status == "pending" {
			s.state.Outbox[i].Attempts++
			entry := s.state.Outbox[i]
			entry.Payload = append(json.RawMessage(nil), s.state.Outbox[i].Payload...)
			out = append(out, entry)
		}
	}
	if len(out) > 0 {
		_ = s.persistLocked(nil)
	}
	return out, nil
}

// AckOutbox confirms remote acceptance; a duplicate ack for an already
// sent entry is a no-op (reconnect safety).
func (s *Supervisor) AckOutbox(principal, entryID, receipt string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Outbox {
		if s.state.Outbox[i].EntryID != entryID {
			continue
		}
		if s.state.Outbox[i].Status == "sent" {
			return nil
		}
		s.state.Outbox[i].Status = "sent"
		s.state.Outbox[i].SentAt = time.Now().UnixNano()
		if len(receipt) <= 256 {
			s.state.Outbox[i].Receipt = receipt
		}
		_ = s.auditLocked(principal, "", "gateway.ack", entryID, "allow", receipt)
		return s.persistLocked(nil)
	}
	return errors.New("outbox entry not found")
}

func (s *Supervisor) ListConversations(channelID string) []MessageConversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []MessageConversation{}
	for _, conversation := range s.state.Conversations {
		if channelID == "" || conversation.ChannelID == channelID {
			out = append(out, conversation)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}
