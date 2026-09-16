// Package sandbox compiles explicit process rights into the Linux launcher ABI.
package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

type Policy struct {
	Read    []string
	Write   []string
	Execute []string
	Network bool
	KeepFD  []int
}

// Args accepts absolute paths and recursive directory patterns only. The launcher
// resolves and pins each rule before entering its immutable Landlock domain.
func (p Policy) Args(command []string) ([]string, error) {
	if len(command) == 0 || !filepath.IsAbs(command[0]) {
		return nil, errors.New("sandbox command must be an absolute executable path")
	}
	args := []string{}
	for _, group := range []struct {
		flag  string
		paths []string
	}{{"--read", p.Read}, {"--write", p.Write}, {"--execute", p.Execute}} {
		for _, path := range group.paths {
			path = strings.TrimSuffix(path, "/**")
			if !filepath.IsAbs(path) || strings.ContainsAny(path, "*?[]\x00") || filepath.Clean(path) != path {
				return nil, fmt.Errorf("unsupported sandbox path %q", path)
			}
			args = append(args, group.flag, path)
		}
	}
	if p.Network {
		args = append(args, "--net")
	}
	for _, fd := range p.KeepFD {
		if fd < 3 || fd > 63 {
			return nil, errors.New("delegated fd must be between 3 and 63")
		}
		args = append(args, "--keep-fd", fmt.Sprint(fd))
	}
	args = append(args, "--")
	return append(args, command...), nil
}
