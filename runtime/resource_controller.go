package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type ResourceLimits struct {
	CPUs        uint64
	MemoryBytes uint64
	PeriodUS    uint64
}

// CgroupController applies kernel-enforced CPU and memory limits through a
// private cgroup v2 leaf. It is optional because macOS and unprivileged Linux
// hosts may not expose a writable cgroup hierarchy.
type CgroupController struct {
	path string
}

func NewCgroupController(root, name string) (*CgroupController, error) {
	if root == "" || name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return nil, errors.New("invalid cgroup root or name")
	}
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0750); err != nil {
		return nil, err
	}
	return &CgroupController{path: path}, nil
}

func (c *CgroupController) Path() string { return c.path }

func (c *CgroupController) Configure(limits ResourceLimits) error {
	if c == nil || c.path == "" {
		return errors.New("resource controller is nil")
	}
	if limits.PeriodUS == 0 {
		limits.PeriodUS = 100000
	}
	if limits.PeriodUS < 1000 || limits.PeriodUS > 1000000 {
		return errors.New("cgroup cpu period is out of range")
	}
	cpu := "max " + strconv.FormatUint(limits.PeriodUS, 10)
	if limits.CPUs > 0 {
		quota := limits.CPUs * limits.PeriodUS
		if quota/limits.PeriodUS != limits.CPUs {
			return errors.New("cgroup cpu quota overflow")
		}
		cpu = fmt.Sprintf("%d %d", quota, limits.PeriodUS)
	}
	if err := os.WriteFile(filepath.Join(c.path, "cpu.max"), []byte(cpu+"\n"), 0640); err != nil {
		return err
	}
	memory := "max"
	if limits.MemoryBytes > 0 {
		memory = strconv.FormatUint(limits.MemoryBytes, 10)
	}
	return os.WriteFile(filepath.Join(c.path, "memory.max"), []byte(memory+"\n"), 0640)
}

func (c *CgroupController) AttachPID(pid int) error {
	if c == nil || pid <= 0 {
		return errors.New("invalid cgroup pid")
	}
	return os.WriteFile(filepath.Join(c.path, "cgroup.procs"), []byte(strconv.Itoa(pid)+"\n"), 0640)
}

// Reclaim asks memcg to reclaim up to bytes immediately. The kernel may
// reclaim less; callers can inspect memory.current afterwards.
func (c *CgroupController) Reclaim(ctx context.Context, bytes uint64) error {
	if c == nil || bytes == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(c.path, "memory.reclaim"), []byte(strconv.FormatUint(bytes, 10)+"\n"), 0640)
}

func (c *CgroupController) Close() error {
	if c == nil || c.path == "" {
		return nil
	}
	err := os.Remove(c.path)
	c.path = ""
	return err
}
