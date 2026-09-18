package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

const maxWakeBindings = 64
const wakeScanBatch = 64

// WakeBinding persists its scan cursor atomically with mailbox deliveries.
// The v0 LSFS source selects artifact commits, excluding runner writebacks.
type WakeBinding struct {
	BindingID     string `json:"binding_id"`
	ApplicationID string `json:"application_id"`
	Kind          string `json:"source_kind"`
	Ref           string `json:"source_ref"`
	Cursor        int64  `json:"cursor"`
	Enabled       bool   `json:"enabled"`
}

type LSFSWakeEvent struct {
	SourceKind string     `json:"source_kind"`
	BindingID  string     `json:"binding_id"`
	Commit     LSFSCommit `json:"commit"`
	Text       string     `json:"text"`
}

func (s *Supervisor) AddLSFSBinding(principal, id, bindingID, ref string, after int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return ErrApplicationNotFound
	}
	if a.State == "tombstoned" || a.State == "retiring" {
		return ErrApplicationRetired
	}
	if bindingID == "" || len(bindingID) > 128 || after < 0 {
		return errors.New("invalid LSFS binding")
	}
	if ref == "" {
		ref = a.ContextID
	}
	key := id + ":" + bindingID
	if _, ok := s.state.Bindings[key]; ok {
		return errors.New("binding already exists")
	}
	count := 0
	for _, b := range s.state.Bindings {
		if b.ApplicationID == id {
			count++
		}
	}
	if count >= maxWakeBindings {
		return errors.New("wake binding limit reached")
	}
	if _, err := s.contexts.Commits(a.OwnerAgent, ref, after, 1); err != nil {
		return err
	}
	s.state.Bindings[key] = WakeBinding{bindingID, id, "lsfs", ref, after, true}
	_ = s.auditLocked(principal, id, "event_source.create", bindingID, "allow", "lsfs artifact commits")
	return s.persistLocked(nil)
}

func (s *Supervisor) SetLSFSBindingEnabled(principal, id, bindingID string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return ErrApplicationNotFound
	}
	if a.State == "tombstoned" || a.State == "retiring" {
		return ErrApplicationRetired
	}
	key := id + ":" + bindingID
	b, ok := s.state.Bindings[key]
	if !ok {
		return errors.New("binding not found")
	}
	if b.Enabled == enabled {
		return nil
	}
	b.Enabled = enabled
	s.state.Bindings[key] = b
	action := "event_source.disable"
	if enabled {
		action = "event_source.bind"
	}
	_ = s.auditLocked(principal, id, action, bindingID, "allow", "")
	return s.persistLocked(nil)
}

func (s *Supervisor) ListLSFSBindings(id string) ([]WakeBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Applications[id]; !ok {
		return nil, ErrApplicationNotFound
	}
	out := []WakeBinding{}
	for _, b := range s.state.Bindings {
		if b.ApplicationID == id {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BindingID < out[j].BindingID })
	return out, nil
}

// ImportArtifact uses a separate key namespace from runner result/checkpoint
// operations. The commit is durable even if the following audit write fails;
// retrying the import returns the same handle and does not emit a second commit.
func (s *Supervisor) ImportArtifact(principal, id, key string, payload json.RawMessage) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.state.Applications[id]
	if !ok {
		return "", ErrApplicationNotFound
	}
	if a.State == "tombstoned" || a.State == "retiring" {
		return "", ErrApplicationRetired
	}
	if key == "" || len(key) > 256 || !json.Valid(payload) || len(payload) > maxTextBytes {
		return "", errors.New("invalid artifact import")
	}
	handle, err := s.contexts.CommitArtifact(a.OwnerAgent, a.ContextID, "artifact.import:"+key, payload)
	if err != nil {
		return "", err
	}
	_ = s.auditLocked(principal, id, "artifact.import", handle, "allow", "")
	if err := s.persistLocked(nil); err != nil {
		return "", err
	}
	return handle, nil
}

func (s *Supervisor) deliverLSFS() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.state.Bindings))
	for key := range s.state.Bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changed := false
	for _, key := range keys {
		b := s.state.Bindings[key]
		if !b.Enabled {
			continue
		}
		a := s.state.Applications[b.ApplicationID]
		if a == nil || a.State == "tombstoned" || a.State == "retiring" {
			b.Enabled = false
			s.state.Bindings[key] = b
			changed = true
			continue
		}
		commits, err := s.contexts.Commits(a.OwnerAgent, b.Ref, b.Cursor, wakeScanBatch)
		if err != nil {
			s.restoreLocked()
			return err
		}
		for _, commit := range commits {
			if commit.Kind == "artifact" {
				event := LSFSWakeEvent{"lsfs", b.BindingID, commit,
					fmt.Sprintf("LSFS artifact committed: context=%s cursor=%d handle=%s", commit.ContextID, commit.Cursor, commit.Handle)}
				payload, err := json.Marshal(event)
				if err == nil {
					_, err = s.enqueueLocked("supervisor", b.ApplicationID, fmt.Sprintf("lsfs:%s:%d", b.BindingID, commit.Cursor), payload)
				}
				if errors.Is(err, ErrMailboxFull) {
					break
				}
				if err != nil {
					s.restoreLocked()
					return err
				}
			}
			b.Cursor = commit.Cursor
			changed = true
		}
		s.state.Bindings[key] = b
	}
	if changed {
		return s.persistLocked(nil)
	}
	return nil
}
