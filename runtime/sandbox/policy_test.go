package sandbox

import (
	"reflect"
	"testing"
)

func TestPolicyArgs(t *testing.T) {
	args, err := (Policy{Read: []string{"/input/**"}, Write: []string{"/output/**"}, Network: true}).Args([]string{"/bin/engine", "--fd", "3"})
	want := []string{"--read", "/input", "--write", "/output", "--net", "--", "/bin/engine", "--fd", "3"}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Fatalf("args=%v err=%v", args, err)
	}
	for _, path := range []string{"relative", "/input/../secret", "/input/*", "/input/**/x", "/input/"} {
		if _, err := (Policy{Read: []string{path}}).Args([]string{"/bin/engine"}); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	if _, err := (Policy{}).Args([]string{"engine"}); err == nil {
		t.Fatal("accepted PATH lookup")
	}
}
