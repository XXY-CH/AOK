package sandbox

import (
	"reflect"
	"testing"
)

func TestCompilePinsRulesAndNetworkPosture(t *testing.T) {
	plan, err := Compile(Manifest{
		FSRead:  []string{"/input/**", "/etc/resolv.conf"},
		FSWrite: []string{"/output/**"},
		Net:     true,
		Tools:   []string{"search"},
	}, "/sbin/aok-sandbox", []string{"/bin/engine", "--fd", "3"}, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--read", "/input", "--read", "/etc/resolv.conf", "--write", "/output",
		"--net", "--keep-fd", "3", "--", "/bin/engine", "--fd", "3"}
	if !reflect.DeepEqual(plan.Args, want) {
		t.Fatalf("args=%v", plan.Args)
	}
	// Network deny-by-default: a manifest without net compiles no --net
	// flag, so the launcher keeps its seccomp socket filter installed.
	offline, err := Compile(Manifest{FSRead: []string{"/input/**"}}, "/sbin/aok-sandbox", []string{"/bin/engine"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range offline.Args {
		if arg == "--net" {
			t.Fatal("offline manifest compiled a network grant")
		}
	}
}

func TestCompileRejectsBadInput(t *testing.T) {
	// Relative and glob rules are refused before the launcher sees them.
	for _, rule := range []string{"relative/**", "/input/../secret", "/input/*", "/in put"} {
		if _, err := Compile(Manifest{FSRead: []string{rule}}, "/sbin/aok-sandbox", []string{"/bin/engine"}, nil); err == nil {
			t.Fatalf("accepted read rule %q", rule)
		}
	}
	// A PATH-lookup engine or launcher is refused.
	if _, err := Compile(Manifest{}, "aok-sandbox", []string{"/bin/engine"}, nil); err == nil {
		t.Fatal("accepted relative launcher")
	}
	if _, err := Compile(Manifest{}, "/sbin/aok-sandbox", []string{"engine"}, nil); err == nil {
		t.Fatal("accepted PATH-lookup engine")
	}
	// Tools never compile into execute rules.
	plan, err := Compile(Manifest{Tools: []string{"search"}}, "/sbin/aok-sandbox", []string{"/bin/engine"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range plan.Args {
		if arg == "--execute" {
			t.Fatal("tool declaration compiled into an execute rule")
		}
	}
}
