package runtime

import "errors"

const (
	maxApplicationMailboxMessages = 256
	maxApplicationMailboxBytes    = 2 << 20
	maxSupervisorMailboxMessages  = 1024
	maxSupervisorMailboxBytes     = 16 << 20
)

var ErrMailboxFull = errors.New("mailbox capacity exhausted; retry after acknowledgements")

type MailboxUsage struct {
	Messages        int   `json:"messages"`
	PayloadBytes    int64 `json:"payload_bytes"`
	MaxMessages     int   `json:"max_messages"`
	MaxPayloadBytes int64 `json:"max_payload_bytes"`
}

type MailboxCapacity struct {
	Application MailboxUsage `json:"application"`
	Supervisor  MailboxUsage `json:"supervisor"`
}

func (s *Supervisor) InspectMailboxCapacity(id string) (MailboxCapacity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Applications[id]; !ok {
		return MailboxCapacity{}, ErrApplicationNotFound
	}
	return s.mailboxCapacityLocked(id), nil
}

// Derive usage from the durable status, so failed writes and recovery cannot
// leave a separate capacity counter out of sync. Retained history is not charged.
func (s *Supervisor) mailboxCapacityLocked(id string) MailboxCapacity {
	capacity := MailboxCapacity{
		Application: MailboxUsage{MaxMessages: maxApplicationMailboxMessages, MaxPayloadBytes: maxApplicationMailboxBytes},
		Supervisor:  MailboxUsage{MaxMessages: maxSupervisorMailboxMessages, MaxPayloadBytes: maxSupervisorMailboxBytes},
	}
	for applicationID, messages := range s.state.Mailbox {
		for _, m := range messages {
			if m.Status != "pending" && m.Status != "claimed" {
				continue
			}
			capacity.Supervisor.Messages++
			capacity.Supervisor.PayloadBytes += int64(len(m.Payload))
			if applicationID == id {
				capacity.Application.Messages++
				capacity.Application.PayloadBytes += int64(len(m.Payload))
			}
		}
	}
	return capacity
}

func (u MailboxUsage) accepts(bytes int) bool {
	return u.Messages < u.MaxMessages && int64(bytes) <= u.MaxPayloadBytes-u.PayloadBytes
}

func (s *Supervisor) expireMailboxLocked(id string) {
	for i := range s.state.Mailbox[id] {
		m := &s.state.Mailbox[id][i]
		if m.Status == "pending" || m.Status == "claimed" {
			m.Status = "expired"
		}
	}
}
