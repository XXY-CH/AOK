package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// The COW fan-out path: application.fork seals the parent context and the
// child references the same CAS pages; divergence after the fork allocates
// independent tails, and taint labels survive the copy.
func TestForkApplicationSharesContextPages(t *testing.T) {
	s := supervisorForTest(t, t.TempDir())
	parent, err := s.CreateApplication("admin", "planner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enqueue("admin", parent.ApplicationID, "plan", json.RawMessage(`{"text":"shared plan"}`)); err != nil {
		t.Fatal(err)
	}
	id, m, err := s.claimTurn()
	if err != nil || id != parent.ApplicationID {
		t.Fatalf("claim: id=%q err=%v", id, err)
	}
	if err = s.executeTurn(context.Background(), EchoProvider{}, id, m); err != nil {
		t.Fatal(err)
	}

	child, err := s.ForkApplication("admin", parent.ApplicationID, "research", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if child.ForkParent != parent.ApplicationID {
		t.Fatalf("fork parent not recorded: %+v", child)
	}
	parentPages, parentTail, err := s.ContextPages(parent.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	childPages, childTail, err := s.ContextPages(child.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(parentPages) == 0 {
		t.Fatal("parent context sealed no pages at fork")
	}
	if len(parentPages) != len(childPages) {
		t.Fatalf("child pages diverge at fork: parent=%d child=%d", len(parentPages), len(childPages))
	}
	for i := range parentPages {
		if parentPages[i] != childPages[i] {
			t.Fatalf("page %d not shared: parent=%s child=%s", i, parentPages[i], childPages[i])
		}
	}
	if parentTail != "" || childTail != "" {
		t.Fatalf("fork left unsealed tails: parent=%q child=%q", parentTail, childTail)
	}

	// A turn on the child diverges its tail only; the parent's sealed
	// pages stay identical.
	if _, err = s.Enqueue("admin", child.ApplicationID, "r0", json.RawMessage(`{"text":"focus area"}`)); err != nil {
		t.Fatal(err)
	}
	cid, cm, err := s.claimTurn()
	if err != nil || cid != child.ApplicationID {
		t.Fatalf("claim child: id=%q err=%v", cid, err)
	}
	if err = s.executeTurn(context.Background(), EchoProvider{}, cid, cm); err != nil {
		t.Fatal(err)
	}
	afterPages, afterTail, err := s.ContextPages(child.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPages) != len(childPages) || afterTail == "" {
		t.Fatalf("child divergence broke sharing or left no tail: pages=%d tail=%q", len(afterPages), afterTail)
	}
	unchangedPages, unchangedTail, err := s.ContextPages(parent.ApplicationID)
	if err != nil {
		t.Fatal(err)
	}
	if len(unchangedPages) != len(parentPages) || unchangedTail != "" {
		t.Fatal("parent context mutated by child turn")
	}
	found := false
	for _, r := range s.Audit() {
		if r.Action == "application.fork" && r.ApplicationID == child.ApplicationID && r.Object == parent.ApplicationID {
			found = true
		}
	}
	if !found {
		t.Fatal("application.fork not audited")
	}
}

func TestForkApplicationInheritsTaint(t *testing.T) {
	s := supervisorForTest(t, t.TempDir())
	parent, err := s.CreateApplication("admin", "planner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Enqueue("admin", parent.ApplicationID, "t1", json.RawMessage(`{"text":"secret","taint":3}`)); err != nil {
		t.Fatal(err)
	}
	id, m, err := s.claimTurn()
	if err != nil || id == "" {
		t.Fatalf("claim: %v", err)
	}
	if err = s.executeTurn(context.Background(), EchoProvider{}, id, m); err != nil {
		t.Fatal(err)
	}
	child, err := s.ForkApplication("admin", parent.ApplicationID, "research", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if child.TaintBits != 3 {
		t.Fatalf("fork lost taint: %+v", child)
	}
	// A fork of a retired parent is refused.
	if err = s.RetireApplication("admin", child.ApplicationID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ForkApplication("admin", child.ApplicationID, "research", "on_event"); !errors.Is(err, ErrApplicationRetired) {
		t.Fatalf("forked from a retired parent: %v", err)
	}
	if _, err = s.ForkApplication("admin", "missing", "research", "on_event"); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("missing parent: %v", err)
	}
}
