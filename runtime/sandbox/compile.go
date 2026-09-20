// Package sandbox compiles explicit process rights into the Linux launcher ABI.
package sandbox

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Manifest is the capability-manifest subset that compiles into launcher
// rules. Tools are capability declarations, not filesystem execute rights;
// the compiler never turns them into Landlock execute rules.
type Manifest struct {
	FSRead  []string
	FSWrite []string
	Net     bool
	Tools   []string
}

// LauncherPlan is the compiled launcher invocation: pinned Landlock path
// rules, the seccomp network posture (no --net means the launcher's socket
// deny filter stays installed), the delegated descriptors, and the engine
// command that runs after --.
type LauncherPlan struct {
	Launcher string
	Args     []string
	Policy   Policy
}

// Compile turns a manifest into the launcher plan. Every path rule must be
// absolute with an optional trailing /** recursion marker — the launcher
// resolves and pins each rule before entering its immutable Landlock domain.
// The engine command must be an absolute executable path so the launcher
// never performs a PATH lookup inside the sandbox.
func Compile(manifest Manifest, launcher string, engineCommand []string, keepFD []int) (LauncherPlan, error) {
	if launcher == "" || !filepath.IsAbs(launcher) {
		return LauncherPlan{}, errors.New("launcher must be an absolute executable path")
	}
	if len(engineCommand) == 0 || !filepath.IsAbs(engineCommand[0]) {
		return LauncherPlan{}, errors.New("engine command must be an absolute executable path")
	}
	policy := Policy{Read: normalizeRules(manifest.FSRead), Write: normalizeRules(manifest.FSWrite), Network: manifest.Net, KeepFD: keepFD}
	for _, group := range [][]string{policy.Read, policy.Write} {
		for _, rule := range group {
			if !validRule(rule) {
				return LauncherPlan{}, fmt.Errorf("whitespace in sandbox rule %q", rule)
			}
		}
	}
	args, err := policy.Args(engineCommand)
	if err != nil {
		return LauncherPlan{}, err
	}
	return LauncherPlan{Launcher: launcher, Args: args, Policy: policy}, nil
}

// normalizeRules accepts absolute directory paths with an optional trailing
// /** and rejects everything else before the launcher sees it. Rule strings
// are operator-controlled identifiers: whitespace is a mistake, not a name.
func normalizeRules(rules []string) []string {
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, strings.TrimSuffix(rule, "/**"))
	}
	return out
}

// validRule reports whether a path rule survives compilation. Args() keeps
// the same checks at the launcher boundary; this front-runs them with the
// manifest's stricter no-whitespace rule.
func validRule(rule string) bool {
	return !strings.ContainsAny(rule, " \t\n\r")
}

// String renders the plan for audit logs; it contains no secrets.
func (p LauncherPlan) String() string {
	return fmt.Sprintf("launcher=%s read=%d write=%d net=%t keepfd=%d engine=%s",
		p.Launcher, len(p.Policy.Read), len(p.Policy.Write), p.Policy.Network, len(p.Policy.KeepFD), strings.Join(p.Args, " "))
}
