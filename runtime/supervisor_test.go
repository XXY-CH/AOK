package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func supervisorForTest(t *testing.T, root string) *Supervisor {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewSupervisor(root, CapabilitySet{FSRead: []string{"/data/in/**"}, FSWrite: []string{"/data/out/**"}, Tools: []string{"search"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSupervisorRestartMailbox(t *testing.T) {
	root := t.TempDir()
	s := supervisorForTest(t, root)
	a, err := s.CreateApplication("admin", "research", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Enqueue("admin", a.ApplicationID, "request-1", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	dup, err := s.Enqueue("admin", a.ApplicationID, "request-1", json.RawMessage(`{"text":"hello"}`))
	if err != nil || dup.MessageID != m.MessageID {
		t.Fatalf("idempotent enqueue: %v %v", dup, err)
	}
	if _, err = s.Enqueue("admin", a.ApplicationID, "request-1", json.RawMessage(`{"text":"different"}`)); !errors.Is(err, ErrDuplicateMessage) {
		t.Fatal(err)
	}
	claimed, err := s.ClaimMailbox("worker", a.ApplicationID)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim %v %v", claimed, err)
	}
	claimed[0].Payload[0] = 'X'
	if _, err = NewSupervisor(root, CapabilitySet{}); err == nil {
		t.Fatal("second writer accepted")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = supervisorForTest(t, root)
	recovered, err := s.InspectApplication(a.ApplicationID)
	if err != nil || recovered != a {
		t.Fatalf("identity changed: %v %v", recovered, err)
	}
	claimed, err = s.ClaimMailbox("worker", a.ApplicationID)
	if err != nil || len(claimed) != 1 || !json.Valid(claimed[0].Payload) {
		t.Fatalf("unacked delivery lost: %v %v", claimed, err)
	}
	if err = s.AckMailbox("worker", a.ApplicationID, m.MessageID); err != nil {
		t.Fatal(err)
	}
	before := len(s.Audit())
	if err = s.AckMailbox("worker", a.ApplicationID, m.MessageID); err != nil || len(s.Audit()) != before {
		t.Fatal("ack not idempotent")
	}
	s.Close()
	s = supervisorForTest(t, root)
	claimed, err = s.ClaimMailbox("worker", a.ApplicationID)
	if err != nil || len(claimed) != 0 {
		t.Fatal("acked delivery replayed")
	}
	if err = s.RetireApplication("admin", a.ApplicationID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enqueue("admin", a.ApplicationID, "new", json.RawMessage(`{}`)); !errors.Is(err, ErrApplicationRetired) {
		t.Fatal(err)
	}
	if _, err = s.ClaimMailbox("worker", a.ApplicationID); !errors.Is(err, ErrApplicationRetired) {
		t.Fatal(err)
	}
	if err = VerifyAudit(s.Audit()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "state.db"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state permissions", err)
	}
}

func TestSupervisorPolicyAndRollback(t *testing.T) {
	s := supervisorForTest(t, t.TempDir())
	a, err := s.CreateApplication("admin", "owner", "manual")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		object, action string
		allow          bool
	}{
		{"net", "connect", false}, {"tool", "search", true}, {"tool", "exec", false},
		{"fs.read", "/data/in/file", true}, {"fs.read", "/data/input/file", false},
		{"fs.write", "/data/in/file", false}, {"fs.write", "/data/out/file", true},
		{"fs.read", "/data/in/../secret", false}, {"fs.read", "relative", false},
	} {
		ok, err := s.Check("worker", a.ApplicationID, tc.object, tc.action)
		if ok != tc.allow || (err != nil && !errors.Is(err, ErrCapabilityDenied)) {
			t.Fatalf("%+v got %v %v", tc, ok, err)
		}
	}
	audit := s.Audit()
	if err = VerifyAudit(audit); err != nil {
		t.Fatal(err)
	}
	audit[0].Principal = "attacker"
	if VerifyAudit(audit) == nil {
		t.Fatal("audit mutation accepted")
	}
	s.db.Close()
	if err = s.RetireApplication("admin", a.ApplicationID); err == nil {
		t.Fatal("write to closed DB accepted")
	}
	after, err := s.InspectApplication(a.ApplicationID)
	if err != nil || after.State != "serving" {
		t.Fatal("failed write leaked into visible state")
	}
}

func TestManifestYAML(t *testing.T) {
	p, err := LoadCapabilitySet("../manifests/research-agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if p.Engine != "echo" || p.Net || len(p.Tools) != 1 || p.Tools[0] != "search" || len(p.FSRead) != 1 {
		t.Fatalf("manifest parsed incorrectly: %+v", p)
	}
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err = os.WriteFile(path, []byte("engine: echo\ncapabilities:\n  nett: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadCapabilitySet(path); err == nil {
		t.Fatal("unknown permission field accepted")
	}
}
