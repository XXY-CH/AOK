package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ResourceSnapshot struct {
	CPUUsageUS uint64 `json:"cpu_usage_us"`
	RSSBytes   uint64 `json:"rss_bytes"`
}

// Snapshot reads the kernel counters for this cgroup. It is deliberately
// separate from Configure so callers can expose measurements without granting
// write access to the cgroup hierarchy.
func (c *CgroupController) Snapshot() (ResourceSnapshot, error) {
	if c == nil || c.path == "" {
		return ResourceSnapshot{}, errors.New("resource controller is nil")
	}
	data, err := readFile(c.path, "memory.current")
	if err != nil {
		return ResourceSnapshot{}, err
	}
	rss, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return ResourceSnapshot{}, errors.New("invalid cgroup memory.current")
	}
	data, err = readFile(c.path, "cpu.stat")
	if err != nil {
		return ResourceSnapshot{}, err
	}
	var cpu uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			cpu, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return ResourceSnapshot{}, errors.New("invalid cgroup cpu.stat")
			}
			break
		}
	}
	return ResourceSnapshot{CPUUsageUS: cpu, RSSBytes: rss}, nil
}

// Monitor enforces the memory pressure edge of a configured resource domain.
// cpu.max remains continuously enforced by the kernel scheduler; this loop
// only samples counters and asks memcg to reclaim excess resident memory.
func (c *CgroupController) Monitor(ctx context.Context, limits ResourceLimits, interval time.Duration, onSample func(ResourceSnapshot)) error {
	if c == nil || c.path == "" {
		return errors.New("resource controller is nil")
	}
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	sample := func() error {
		current, err := c.Snapshot()
		if err != nil {
			return err
		}
		if onSample != nil {
			onSample(current)
		}
		if limits.MemoryBytes > 0 && current.RSSBytes > limits.MemoryBytes {
			return c.Reclaim(ctx, current.RSSBytes-limits.MemoryBytes)
		}
		return nil
	}
	if err := sample(); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := sample(); err != nil {
				return err
			}
		}
	}
}

func readFile(root, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(root, name))
}
