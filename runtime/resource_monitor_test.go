package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestCgroupMonitorSnapshotsAndReclaims(t *testing.T) {
	root := t.TempDir()
	for name, data := range map[string]string{
		"memory.current": "8192\n",
		"cpu.stat":       "usage_usec 42\nuser_usec 40\n",
		"memory.reclaim": "",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0640); err != nil {
			t.Fatal(err)
		}
	}
	c := &CgroupController{path: root}
	var mu sync.Mutex
	var seen []ResourceSnapshot
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Millisecond, cancel)
	err := c.Monitor(ctx, ResourceLimits{MemoryBytes: 4096}, time.Hour, func(s ResourceSnapshot) {
		mu.Lock()
		seen = append(seen, s)
		mu.Unlock()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("monitor error=%v", err)
	}
	if len(seen) != 1 || seen[0].RSSBytes != 8192 || seen[0].CPUUsageUS != 42 {
		t.Fatalf("snapshots=%+v", seen)
	}
	reclaim, _ := os.ReadFile(filepath.Join(root, "memory.reclaim"))
	if string(reclaim) != "4096\n" {
		t.Fatalf("reclaim=%q", reclaim)
	}
}
