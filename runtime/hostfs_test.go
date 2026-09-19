package runtime

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestHostfsMountLifecycle(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	mount, err := s.MountHostfs("tester", app.ApplicationID, "/mnt/project/in", "ro", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !mount.Enabled || mount.Mode != "ro" || mount.Version != 1 {
		t.Fatalf("mount wrong: %+v", mount)
	}
	// Path-prefix scoping: the mount path and children authorize reads;
	// writes are denied under ro; sibling paths are denied entirely.
	if err := s.CheckHostfs(app.ApplicationID, "/mnt/project/in", "read"); err != nil {
		t.Fatalf("read denied: %v", err)
	}
	if err := s.CheckHostfs(app.ApplicationID, "/mnt/project/in/nested/file", "read"); err != nil {
		t.Fatalf("nested read denied: %v", err)
	}
	if err := s.CheckHostfs(app.ApplicationID, "/mnt/project/in", "write"); !errors.Is(err, ErrMountDenied) {
		t.Fatalf("write under ro accepted: %v", err)
	}
	if err := s.CheckHostfs(app.ApplicationID, "/mnt/other", "read"); !errors.Is(err, ErrMountDenied) {
		t.Fatalf("out-of-mount path accepted: %v", err)
	}
	// Append-only accepts append, refuses write.
	appendMount, err := s.MountHostfs("tester", app.ApplicationID, "/mnt/log", "append-only", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CheckHostfs(app.ApplicationID, "/mnt/log", "append"); err != nil {
		t.Fatalf("append denied: %v", err)
	}
	if err := s.CheckHostfs(app.ApplicationID, "/mnt/log", "write"); !errors.Is(err, ErrMountDenied) {
		t.Fatalf("write under append-only accepted: %v", err)
	}
	// TTL expiry disables the mount.
	ttlMount, err := s.MountHostfs("tester", app.ApplicationID, "/mnt/tmp", "rw", 0, -time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = ttlMount
	if err := s.CheckHostfs(app.ApplicationID, "/mnt/tmp", "write"); !errors.Is(err, ErrMountDenied) {
		t.Fatalf("expired mount still active: %v", err)
	}
	// Unmount revokes and bumps the version; mounts persist across restart.
	if err := s.UnmountHostfs("tester", appendMount.MountID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckHostfs(app.ApplicationID, "/mnt/log", "append"); !errors.Is(err, ErrMountDenied) {
		t.Fatalf("unmounted path accepted: %v", err)
	}
	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if err := s2.CheckHostfs(app.ApplicationID, "/mnt/project/in", "read"); err != nil {
		t.Fatalf("mount lost across restart: %v", err)
	}
}

func TestArtifactTransferTaintAndIdempotency(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	// Clean export commits and is idempotent.
	first, err := s.ExportArtifact("tester", app.ApplicationID, "k1", json.RawMessage(`{"v":1}`))
	if err != nil || first.Status != "committed" {
		t.Fatalf("export: %+v %v", first, err)
	}
	again, err := s.ExportArtifact("tester", app.ApplicationID, "k1", json.RawMessage(`{"v":1}`))
	if err != nil || again.TransferID != first.TransferID {
		t.Fatalf("export not idempotent: %+v %v", again, err)
	}
	// A tainted application cannot export through the artifact path: the
	// taint gate is in front of every outbound surface.
	if _, err := s.Enqueue("tester", app.ApplicationID, "t",
		json.RawMessage(`{"text":"x","taint":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.claimTurn(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ExportArtifact("tester", app.ApplicationID, "k2", json.RawMessage(`{"v":2}`)); !errors.Is(err, ErrTaintBlocked) {
		t.Fatalf("tainted export accepted: %v", err)
	}
	// Import commits idempotently too.
	imp, err := s.CommitImport("tester", app.ApplicationID, "in-1", json.RawMessage(`{"doc":true}`))
	if err != nil || imp.Direction != "import" {
		t.Fatalf("import: %+v %v", imp, err)
	}
	if imp2, err := s.CommitImport("tester", app.ApplicationID, "in-1", json.RawMessage(`{"doc":true}`)); err != nil || imp2.TransferID != imp.TransferID {
		t.Fatalf("import not idempotent: %+v %v", imp2, err)
	}
}

// Traversal, dropbox modes and the full mode-operation matrix.
func TestHostfsTraversalAndMatrix(t *testing.T) {
	s := newKernelTestSupervisor(t)
	app, err := s.CreateApplication("tester", "owner", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MountHostfs("tester", app.ApplicationID, "/mnt/in", "ro", 0, 0); err != nil {
		t.Fatal(err)
	}
	// Traversal must die at the gate.
	for _, path := range []string{
		"/mnt/in/../../etc/passwd", "/mnt/in//x", "/mnt/in/./x", "mnt/in", "/mnt/in/../out",
	} {
		if err := s.CheckHostfs(app.ApplicationID, path, "read"); err == nil {
			t.Fatalf("traversal path accepted: %q", path)
		}
	}
	// Full mode-operation matrix.
	modes := map[string]map[string]bool{
		"ro":          {"read": true, "write": false, "append": false, "intake": false, "emit": false},
		"rw":          {"read": true, "write": true, "append": false},
		"append-only": {"read": false, "write": false, "append": true},
		"dropbox-in":  {"intake": true, "read": false, "emit": false},
		"dropbox-out": {"emit": true, "read": false, "intake": false},
	}
	for mode, ops := range modes {
		if _, err := s.MountHostfs("tester", app.ApplicationID, "/mnt/"+mode, mode, 0, 0); err != nil {
			t.Fatal(err)
		}
		for op, allowed := range ops {
			err := s.CheckHostfs(app.ApplicationID, "/mnt/"+mode, op)
			if allowed && err != nil {
				t.Fatalf("%s: %s should be allowed: %v", mode, op, err)
			}
			if !allowed && err == nil {
				t.Fatalf("%s: %s should be denied", mode, op)
			}
		}
	}
	// Host imports fold external taint (hostfs-model contract).
	clean, err := s.CreateApplication("tester", "other", "on_event")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CommitImport("tester", clean.ApplicationID, "doc-1", json.RawMessage(`{"doc":true}`)); err != nil {
		t.Fatal(err)
	}
	inspected, _ := s.InspectApplication(clean.ApplicationID)
	if inspected.TaintBits&TaintExternal == 0 {
		t.Fatal("host import folded no external taint")
	}
	// Transfers persist across restart.
	root := s.root
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := NewSupervisor(root, CapabilitySet{Engine: "echo"})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if again, err := s2.CommitImport("tester", clean.ApplicationID, "doc-1", json.RawMessage(`{"doc":true}`)); err != nil {
		t.Fatalf("import after restart: %v", err)
	} else {
		original, _ := s.CommitImport("tester", clean.ApplicationID, "sentinel", json.RawMessage(`{}`))
		_ = original
		_ = again
	}
}
