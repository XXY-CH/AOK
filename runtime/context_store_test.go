package runtime

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestContextStoreForkCOWCheckpointReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	s, err := OpenContextStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Create("alice", []byte("system prefix"))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := s.Append("alice", id, "append-1", []byte("first turn"))
	if err != nil {
		t.Fatal(err)
	}
	child1, err := s.Fork("alice", id, "fork-1")
	if err != nil {
		t.Fatal(err)
	}
	child2, err := s.Fork("alice", id, "fork-2")
	if err != nil {
		t.Fatal(err)
	}
	stats, err := s.Stats("alice", id, child1, child2)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Contexts != 3 || stats.UniqueBlobs != 2 || stats.SharedBlobs != 2 || stats.References != 6 || stats.SavedBytes != 46 {
		t.Fatalf("unexpected sharing: %+v", stats)
	}
	if _, err = s.Append("alice", child1, "child-tail", []byte("branch one")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append("alice", child2, "child-tail", []byte("branch two")); err != nil {
		t.Fatal(err)
	}
	parent, err := s.Snapshot("alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Tail != nil || len(parent.Prefix) != 2 || parent.Prefix[1].Hash != tail {
		t.Fatalf("parent modified: %+v", parent)
	}
	checkpoint, err := s.Checkpoint("alice", child1, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append("alice", child1, "after-checkpoint", []byte(" discard")); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenContextStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Restore("alice", child1, "restore", checkpoint); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot("alice", child1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.ReadPage("alice", child1, snap.Tail.Hash)
	if err != nil || string(data) != "branch one" {
		t.Fatalf("restore data=%q err=%v", data, err)
	}
	other, err := s.Snapshot("alice", child2)
	if err != nil {
		t.Fatal(err)
	}
	data, err = s.ReadPage("alice", child2, other.Tail.Hash)
	if err != nil || string(data) != "branch two" {
		t.Fatalf("COW data=%q err=%v", data, err)
	}
	info, err := os.Stat(filepath.Join(root, "contexts.db"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("database mode: %v %v", info, err)
	}
}

func TestContextStoreArtifactScopeAndReplay(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private")
	s, err := OpenContextStore(root)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Create("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Create("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.Create("bob", nil)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := s.CommitArtifact("alice", id, "side-effect", []byte("result"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenContextStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	replayed, err := s.CommitArtifact("alice", id, "side-effect", []byte("result"))
	if err != nil || replayed != handle {
		t.Fatalf("replay %q %v", replayed, err)
	}
	if _, err = s.CommitArtifact("alice", id, "side-effect", []byte("changed")); !errors.Is(err, ErrContextIdempotency) {
		t.Fatalf("input mismatch accepted: %v", err)
	}
	for _, item := range []struct{ owner, id, handle string }{{"bob", id, handle}, {"alice", other, handle}, {"alice", foreign, handle}, {"alice", id, filepath.Join(root, "contexts.db")}} {
		if _, err = s.ReadArtifact(item.owner, item.id, item.handle); !errors.Is(err, ErrContextAccess) {
			t.Fatalf("unauthorized read %+v: %v", item, err)
		}
	}
	data, err := s.ReadArtifact("alice", id, handle)
	if err != nil || !bytes.Equal(data, []byte("result")) {
		t.Fatalf("artifact read %q %v", data, err)
	}
	events, err := s.Commits("alice", id, 0, 100)
	if err != nil || len(events) != 1 || events[0].Kind != "artifact" {
		t.Fatalf("duplicate commit: %+v %v", events, err)
	}
	if _, err = s.Commits("bob", id, 0, 100); !errors.Is(err, ErrContextAccess) {
		t.Fatalf("unauthorized events: %v", err)
	}
	next, err := s.Commits("alice", id, events[0].Cursor, 100)
	if err != nil || len(next) != 0 {
		t.Fatalf("cursor %+v %v", next, err)
	}
	if _, err = s.Append("alice", id, "side-effect", []byte("result")); !errors.Is(err, ErrContextIdempotency) {
		t.Fatalf("cross-operation key accepted: %v", err)
	}
}

func TestContextStoreDetectsCorruptionAndRollsBack(t *testing.T) {
	s, err := OpenContextStore(filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.Create("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := s.Append("alice", id, "tail", []byte("good"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitArtifact("alice", id, "artifact", []byte("good")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE blobs SET data=? WHERE hash=?", []byte("bad"), hash); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadPage("alice", id, hash); !errors.Is(err, ErrContextCorrupt) {
		t.Fatalf("page corruption: %v", err)
	}
	if _, err = s.ReadArtifact("alice", id, hash); !errors.Is(err, ErrContextCorrupt) {
		t.Fatalf("artifact corruption: %v", err)
	}
	if _, err = s.Append("alice", id, "retryable", []byte("next")); !errors.Is(err, ErrContextCorrupt) {
		t.Fatalf("append corruption: %v", err)
	}
	if _, err = s.db.Exec("UPDATE blobs SET data=? WHERE hash=?", []byte("good"), hash); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append("alice", id, "retryable", []byte("next")); err != nil {
		t.Fatalf("failed transaction reserved key: %v", err)
	}
	events, err := s.Commits("alice", id, 0, 100)
	if err != nil || len(events) != 3 {
		t.Fatalf("failed transaction emitted commit: %+v %v", events, err)
	}
}

func TestContextStoreAppendAndForkIdempotency(t *testing.T) {
	s, err := OpenContextStore(filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id, err := s.Create("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Append("alice", id, "append", []byte("once"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Append("alice", id, "append", []byte("once"))
	if err != nil || first != again {
		t.Fatalf("append replay: %v", err)
	}
	child, err := s.Fork("alice", id, "fork")
	if err != nil {
		t.Fatal(err)
	}
	again, err = s.Fork("alice", id, "fork")
	if err != nil || child != again {
		t.Fatalf("fork replay: %v", err)
	}
	data, err := s.ReadPage("alice", child, first)
	if err != nil || string(data) != "once" {
		t.Fatalf("duplicate append: %q %v", data, err)
	}
	if _, err = s.ReadPage("bob", child, first); !errors.Is(err, ErrContextAccess) {
		t.Fatalf("unauthorized page: %v", err)
	}
}
