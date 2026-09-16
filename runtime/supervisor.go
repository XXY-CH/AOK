package runtime

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// Supervisor is the durable user-space half of the AOK core. The kernel owns
// current AID/aproc objects; this component owns stable Application identity,
// mailbox replay and policy/audit state across worker or VM restarts.
type Supervisor struct {
	mu           sync.Mutex
	root         string
	state        supervisorState
	policy       CapabilitySet
	previousHash string
	db           *sql.DB
	lock         *os.File
	committed    []byte
	contexts     *ContextStore
}

type supervisorState struct {
	Applications map[string]*Application     `json:"applications"`
	Mailbox      map[string][]MailboxMessage `json:"mailbox"`
	Audit        []AuditRecord               `json:"audit"`
	NextSequence uint64                      `json:"next_sequence"`
	Results      map[string]TurnResult       `json:"results"`
	Prepared     map[string]TurnResult       `json:"prepared"`
	Timers       map[string]ApplicationTimer `json:"timers"`
}

type Application struct {
	ApplicationID   string `json:"application_id"`
	OwnerAgent      string `json:"owner_agent"`
	ContractVersion uint64 `json:"contract_version"`
	State           string `json:"state"`
	WakePolicy      string `json:"wake_policy"`
	Checkpoint      string `json:"checkpoint_ref,omitempty"`
	Generation      uint64 `json:"generation"`
	CreatedAt       int64  `json:"created_at"`
	RetiredAt       int64  `json:"retired_at,omitempty"`
	TokensUsed      uint64 `json:"tokens_used"`
	TokenLimit      uint64 `json:"token_limit"`
	Failures        uint32 `json:"failures"`
	ContextID       string `json:"context_id"`
}

type MailboxMessage struct {
	MessageID      string          `json:"message_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Payload        json.RawMessage `json:"payload"`
	Status         string          `json:"status"`
	Sequence       uint64          `json:"sequence"`
	CreatedAt      int64           `json:"created_at"`
	Attempts       uint32          `json:"attempts"`
}

type CapabilitySet struct {
	Engine  string   `json:"engine"`
	FSRead  []string `json:"fs_read,omitempty"`
	FSWrite []string `json:"fs_write,omitempty"`
	Net     bool     `json:"net"`
	Tools   []string `json:"tools,omitempty"`
}

type AuditRecord struct {
	Sequence      uint64 `json:"sequence"`
	Time          int64  `json:"time"`
	Principal     string `json:"principal"`
	ApplicationID string `json:"application_id,omitempty"`
	Action        string `json:"action"`
	Object        string `json:"object"`
	Decision      string `json:"decision"`
	Reason        string `json:"reason,omitempty"`
	PreviousHash  string `json:"previous_hash"`
	Hash          string `json:"hash"`
}

var (
	ErrApplicationNotFound = errors.New("application not found")
	ErrApplicationRetired  = errors.New("application is retired")
	ErrDuplicateMessage    = errors.New("duplicate mailbox idempotency key")
	ErrCapabilityDenied    = errors.New("capability denied")
)

func NewSupervisor(root string, policy CapabilitySet) (*Supervisor, error) {
	if root == "" {
		return nil, errors.New("supervisor state directory is required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must be a private directory (0700)")
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("supervisor state already in use")
	}
	policy.FSRead = append([]string(nil), policy.FSRead...)
	policy.FSWrite = append([]string(nil), policy.FSWrite...)
	policy.Tools = append([]string(nil), policy.Tools...)
	s := &Supervisor{root: root, policy: policy, state: supervisorState{Applications: map[string]*Application{}, Mailbox: map[string][]MailboxMessage{}}}
	s.lock = lock
	if err := s.load(); err != nil {
		s.Close()
		return nil, err
	}
	s.contexts, err = OpenContextStore(filepath.Join(root, "contexts"))
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Supervisor) load() error {
	path := filepath.Join(s.root, "state.db")
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		return errors.New("state database must be a private regular file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	f.Close()
	s.db, err = sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	s.db.SetMaxOpenConns(1)
	if _, err = s.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
	CREATE TABLE IF NOT EXISTS supervisor_state (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL);`); err != nil {
		return err
	}
	var b []byte
	err = s.db.QueryRow("SELECT data FROM supervisor_state WHERE id=1").Scan(&b)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(b) > 0 {
		if err = json.Unmarshal(b, &s.state); err != nil {
			return err
		}
	}
	if s.state.Applications == nil || s.state.Mailbox == nil {
		return errors.New("invalid supervisor state")
	}
	if s.state.Results == nil {
		s.state.Results = map[string]TurnResult{}
	}
	if s.state.Prepared == nil {
		s.state.Prepared = map[string]TurnResult{}
	}
	if s.state.Timers == nil {
		s.state.Timers = map[string]ApplicationTimer{}
	}
	if err = VerifyAudit(s.state.Audit); err != nil {
		return err
	}
	if n := len(s.state.Audit); n > 0 {
		s.previousHash = s.state.Audit[n-1].Hash
	}
	s.committed, _ = json.Marshal(s.state)
	// Claims belong to the previous worker incarnation. Unacked deliveries are
	// replayed at least once; consumers must commit effects using the message ID.
	for id, messages := range s.state.Mailbox {
		for i := range messages {
			if messages[i].Status == "claimed" {
				s.state.Mailbox[id][i].Status = "pending"
			}
		}
	}
	return s.persistLocked(nil)
}

func (s *Supervisor) persistLocked(_ any) error {
	b, err := json.Marshal(s.state)
	if err == nil {
		_, err = s.db.Exec("INSERT INTO supervisor_state(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", b)
	}
	if err != nil {
		s.restoreLocked()
		return err
	}
	s.committed = b
	return nil
}

func (s *Supervisor) restoreLocked() {
	s.state = supervisorState{}
	_ = json.Unmarshal(s.committed, &s.state)
	s.previousHash = ""
	if n := len(s.state.Audit); n > 0 {
		s.previousHash = s.state.Audit[n-1].Hash
	}
}

func (s *Supervisor) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.contexts != nil {
		_ = s.contexts.Close()
	}
	if s.lock != nil {
		_ = unix.Flock(int(s.lock.Fd()), unix.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
	return err
}

func auditHash(r AuditRecord) string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// VerifyAudit detects record mutation, reordering and gaps in the hash chain.
// Detecting truncation requires an externally retained head, not just this log.
func VerifyAudit(records []AuditRecord) error {
	previous := ""
	var seq uint64
	for _, r := range records {
		if r.Sequence <= seq || r.PreviousHash != previous || r.Hash != auditHash(r) {
			return errors.New("invalid audit chain")
		}
		previous = r.Hash
		seq = r.Sequence
	}
	return nil
}

func (s *Supervisor) auditLocked(principal, applicationID, action, object, decision, reason string) error {
	s.state.NextSequence++
	r := AuditRecord{Sequence: s.state.NextSequence, Time: time.Now().UnixNano(), Principal: principal, ApplicationID: applicationID, Action: action, Object: object, Decision: decision, Reason: reason, PreviousHash: s.previousHash}
	r.Hash = auditHash(r)
	s.previousHash = r.Hash
	s.state.Audit = append(s.state.Audit, r)
	return nil
}

func (s *Supervisor) CreateApplication(principal, owner, wake string) (Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" || len(owner) > 256 {
		return Application{}, errors.New("invalid owner")
	}
	if wake == "" {
		wake = "on_event"
	}
	if wake != "on_event" && wake != "manual" && wake != "on_quiescent" {
		return Application{}, errors.New("invalid wake policy")
	}
	var idBytes [16]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return Application{}, err
	}
	idBytes[6] = (idBytes[6] & 0x0f) | 0x40
	idBytes[8] = (idBytes[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", idBytes[0:4], idBytes[4:6], idBytes[6:8], idBytes[8:10], idBytes[10:16])
	a := Application{ApplicationID: id, OwnerAgent: owner, ContractVersion: 1, State: "serving", WakePolicy: wake, Generation: 1, CreatedAt: time.Now().UnixNano()}
	contextID, err := s.contexts.Create(owner, nil)
	if err != nil {
		return Application{}, err
	}
	a.ContextID = contextID
	s.state.Applications[id] = &a
	s.state.Mailbox[id] = nil
	if err := s.auditLocked(principal, id, "application.create", id, "allow", ""); err != nil {
		return Application{}, err
	}
	if err := s.persistLocked(map[string]any{"type": "application.create", "application_id": id}); err != nil {
		return Application{}, err
	}
	return a, nil
}

func (s *Supervisor) InspectApplication(id string) (Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return Application{}, ErrApplicationNotFound
	}
	return *a, nil
}

// ListApplications returns stable snapshots for the local control console.
// Callers receive values detached from supervisor-owned state.
func (s *Supervisor) ListApplications() []Application {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Application, 0, len(s.state.Applications))
	for _, a := range s.state.Applications {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ApplicationID < out[j].ApplicationID
		}
		return out[i].CreatedAt < out[j].CreatedAt
	})
	return out
}

// ListMailbox returns a detached mailbox snapshot, including acknowledged
// records so the console can show durable delivery history.
func (s *Supervisor) ListMailbox(id string) ([]MailboxMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Applications[id]; !ok {
		return nil, ErrApplicationNotFound
	}
	out := make([]MailboxMessage, len(s.state.Mailbox[id]))
	for i, m := range s.state.Mailbox[id] {
		out[i] = m
		out[i].Payload = append(json.RawMessage(nil), m.Payload...)
	}
	return out, nil
}

// ListTimers returns stable timer snapshots for the local control console.
func (s *Supervisor) ListTimers(id string) ([]ApplicationTimer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Applications[id]; !ok {
		return nil, ErrApplicationNotFound
	}
	out := make([]ApplicationTimer, 0)
	for _, timer := range s.state.Timers {
		if timer.ApplicationID != id {
			continue
		}
		timer.Payload = append(json.RawMessage(nil), timer.Payload...)
		out = append(out, timer)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimerID < out[j].TimerID })
	return out, nil
}

func (s *Supervisor) RetireApplication(principal, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return ErrApplicationNotFound
	}
	if a.State == "tombstoned" {
		return nil
	}
	a.State = "retiring"
	a.RetiredAt = time.Now().UnixNano()
	if err := s.auditLocked(principal, id, "application.retire", id, "allow", ""); err != nil {
		return err
	}
	a.State = "tombstoned"
	return s.persistLocked(map[string]any{"type": "application.retire", "application_id": id})
}

func (s *Supervisor) Enqueue(principal, id, key string, payload json.RawMessage) (MailboxMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return MailboxMessage{}, ErrApplicationNotFound
	}
	if a.State == "tombstoned" || a.State == "retiring" {
		return MailboxMessage{}, ErrApplicationRetired
	}
	if key == "" || len(key) > 256 {
		return MailboxMessage{}, errors.New("invalid idempotency key")
	}
	if !json.Valid(payload) || len(payload) > maxTextBytes {
		return MailboxMessage{}, errors.New("invalid mailbox payload")
	}
	for _, m := range s.state.Mailbox[id] {
		if m.IdempotencyKey == key {
			if !bytes.Equal(m.Payload, payload) {
				return MailboxMessage{}, ErrDuplicateMessage
			}
			m.Payload = append(json.RawMessage(nil), m.Payload...)
			return m, nil
		}
	}
	s.state.NextSequence++
	m := MailboxMessage{MessageID: fmt.Sprintf("msg-%d", s.state.NextSequence), IdempotencyKey: key, Payload: append(json.RawMessage(nil), payload...), Status: "pending", Sequence: s.state.NextSequence, CreatedAt: time.Now().UnixNano()}
	s.state.Mailbox[id] = append(s.state.Mailbox[id], m)
	if err := s.auditLocked(principal, id, "mailbox.enqueue", key, "allow", ""); err != nil {
		return MailboxMessage{}, err
	}
	if err := s.persistLocked(map[string]any{"type": "mailbox.enqueue", "application_id": id, "message_id": m.MessageID}); err != nil {
		return MailboxMessage{}, err
	}
	m.Payload = append(json.RawMessage(nil), m.Payload...)
	return m, nil
}

func (s *Supervisor) ClaimMailbox(principal, id string) ([]MailboxMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return nil, ErrApplicationNotFound
	}
	if a.State == "tombstoned" {
		return nil, ErrApplicationRetired
	}
	var out []MailboxMessage
	for i := range s.state.Mailbox[id] {
		if s.state.Mailbox[id][i].Status == "pending" {
			s.state.Mailbox[id][i].Status = "claimed"
			m := s.state.Mailbox[id][i]
			m.Payload = append(json.RawMessage(nil), m.Payload...)
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		_ = s.auditLocked(principal, id, "mailbox.claim", id, "allow", "")
		if err := s.persistLocked(map[string]any{"type": "mailbox.claim", "application_id": id}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Supervisor) AckMailbox(principal, id, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Mailbox[id] {
		if s.state.Mailbox[id][i].MessageID == messageID {
			if s.state.Mailbox[id][i].Status == "acked" {
				return nil
			}
			if s.state.Mailbox[id][i].Status != "claimed" {
				return errors.New("message is not claimed")
			}
			s.state.Mailbox[id][i].Status = "acked"
			if err := s.auditLocked(principal, id, "mailbox.ack", messageID, "allow", ""); err != nil {
				return err
			}
			return s.persistLocked(map[string]any{"type": "mailbox.ack", "message_id": messageID})
		}
	}
	return errors.New("mailbox message not found")
}

func (s *Supervisor) Check(principal, applicationID, object, action string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed := false
	reason := "policy"
	switch object {
	case "net":
		allowed = s.policy.Net
	case "tool":
		for _, v := range s.policy.Tools {
			if v == action {
				allowed = true
			}
		}
	case "fs.read":
		allowed = allowedPath(s.policy.FSRead, action)
	case "fs.write":
		allowed = allowedPath(s.policy.FSWrite, action)
	default:
		reason = "unknown object"
	}
	if a, ok := s.state.Applications[applicationID]; !ok || a.State == "tombstoned" {
		allowed = false
		reason = "application unavailable"
	}
	decision := "deny"
	if allowed {
		decision = "allow"
		reason = ""
	}
	if err := s.auditLocked(principal, applicationID, action, object, decision, reason); err != nil {
		return false, err
	}
	if err := s.persistLocked(map[string]any{"type": "capability.check", "application_id": applicationID, "object": object, "action": action, "decision": decision}); err != nil {
		return false, err
	}
	if !allowed {
		return false, ErrCapabilityDenied
	}
	return true, nil
}

func (s *Supervisor) Audit() []AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditRecord(nil), s.state.Audit...)
}

func LoadCapabilitySet(path string) (CapabilitySet, error) {
	f, err := os.Open(path)
	if err != nil {
		return CapabilitySet{}, err
	}
	defer f.Close()
	var m struct {
		Engine       string `yaml:"engine"`
		Capabilities struct {
			FS struct {
				Read  []string `yaml:"read"`
				Write []string `yaml:"write"`
			} `yaml:"fs"`
			Net   bool     `yaml:"net"`
			Tools []string `yaml:"tools"`
		} `yaml:"capabilities"`
		Resources struct {
			MemMiB int `yaml:"mem_mib"`
			CPUs   int `yaml:"cpus"`
		} `yaml:"resources"`
	}
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err = d.Decode(&m); err != nil {
		return CapabilitySet{}, err
	}
	c := CapabilitySet{Engine: m.Engine, FSRead: m.Capabilities.FS.Read, FSWrite: m.Capabilities.FS.Write, Net: m.Capabilities.Net, Tools: m.Capabilities.Tools}
	if c.Engine == "" {
		return c, errors.New("manifest engine is required")
	}
	for _, p := range append(append([]string(nil), c.FSRead...), c.FSWrite...) {
		base := strings.TrimSuffix(p, "/**")
		if !filepath.IsAbs(base) || filepath.Clean(base) != base || strings.ContainsAny(base, "*?[") {
			return c, errors.New("filesystem rules require absolute paths with optional /** suffix")
		}
	}
	return c, nil
}
func allowedPath(rules []string, path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for _, rule := range rules {
		base := strings.TrimSuffix(rule, "/**")
		if path == base || (base != rule && strings.HasPrefix(path, strings.TrimSuffix(base, "/")+"/")) {
			return true
		}
	}
	return false
}
