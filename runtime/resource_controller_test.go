package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCgroupControllerConfiguresAndAttaches(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "aok")
	if err := os.Mkdir(path, 0750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cpu.max", "memory.max", "cgroup.procs", "memory.reclaim"} {
		if err := os.WriteFile(filepath.Join(path, name), nil, 0640); err != nil {
			t.Fatal(err)
		}
	}
	c := &CgroupController{path: path}
	if err := c.Configure(ResourceLimits{CPUs: 2, MemoryBytes: 1 << 20, PeriodUS: 100000}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(path, "cpu.max"))
	if string(data) != "200000 100000\n" {
		t.Fatalf("cpu.max=%q", data)
	}
	data, _ = os.ReadFile(filepath.Join(path, "memory.max"))
	if string(data) != "1048576\n" {
		t.Fatalf("memory.max=%q", data)
	}
	if err := c.AttachPID(42); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if string(data) != "42\n" {
		t.Fatalf("cgroup.procs=%q", data)
	}
	if err := c.Reclaim(context.Background(), 4096); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(path, "memory.reclaim"))
	if string(data) != "4096\n" {
		t.Fatalf("memory.reclaim=%q", data)
	}
	if _, err := NewCgroupController(root, "../escape"); err == nil {
		t.Fatal("accepted cgroup traversal")
	}
	if _, err := NewCgroupController(root, "bad/name"); err == nil {
		t.Fatal("accepted cgroup separator")
	}
	if strings.Contains(c.Path(), "..") {
		t.Fatal("controller escaped root")
	}
}
