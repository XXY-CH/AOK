package runtime

import (
	"context"
	"errors"
	"testing"
)

type failingProvider struct{ name string }

func (p failingProvider) Name() string { return p.name }
func (p failingProvider) Complete(context.Context, string) (string, Usage, error) {
	return "", Usage{}, errors.New("unavailable")
}

type fixedProvider struct{ name, text string }

func (p fixedProvider) Name() string { return p.name }
func (p fixedProvider) Complete(context.Context, string) (string, Usage, error) {
	return p.text, Usage{InputTokens: 2, OutputTokens: 1}, nil
}

func TestRouterFallsBackInPolicyOrder(t *testing.T) {
	r, err := NewRouterProvider(failingProvider{"llama.cpp"}, fixedProvider{"anthropic", "ok"})
	if err != nil {
		t.Fatal(err)
	}
	text, usage, err := r.Complete(context.Background(), "prompt")
	if err != nil || text != "ok" || usage.OutputTokens != 1 {
		t.Fatalf("text=%q usage=%+v err=%v", text, usage, err)
	}
}

func TestRouterRejectsDuplicateOrEmptyProviders(t *testing.T) {
	if _, err := NewRouterProvider(); err == nil {
		t.Fatal("accepted empty router")
	}
	if _, err := NewRouterProvider(failingProvider{"echo"}, failingProvider{"echo"}); err == nil {
		t.Fatal("accepted duplicate provider")
	}
}

func TestRouterUsesApplicationOrder(t *testing.T) {
	for _, failLocal := range []bool{false, true} {
		var local Provider = fixedProvider{"llama.cpp", "local"}
		if failLocal {
			local = failingProvider{"llama.cpp"}
		}
		r, err := NewRouterProvider(fixedProvider{"anthropic", "cloud"}, local)
		if err != nil {
			t.Fatal(err)
		}
		ctx := WithRouteBackends(context.Background(), []string{"unknown", "llama.cpp", "llama.cpp", "anthropic"})
		text, _, err := r.Complete(ctx, "prompt")
		want, fallbacks := "local", uint32(0)
		if failLocal {
			want, fallbacks = "cloud", 1
		}
		if err != nil || text != want || r.LastRoute().Fallbacks != fallbacks {
			t.Fatalf("local failure=%v text=%q route=%+v err=%v", failLocal, text, r.LastRoute(), err)
		}
	}
}

type keyedProvider struct {
	name, text, key string
}

func (p keyedProvider) Name() string { return p.name }
func (p keyedProvider) Complete(context.Context, string) (string, Usage, error) {
	return p.text, Usage{InputTokens: 2, OutputTokens: 1}, nil
}
func (p keyedProvider) CompatKey() string { return p.key }

func TestRouterRecordsRouteAndFallbackCount(t *testing.T) {
	r, err := NewRouterProvider(failingProvider{"llama.cpp"},
		keyedProvider{name: "anthropic", text: "ok", key: "anthropic|m|messages"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.Complete(context.Background(), "prompt"); err != nil {
		t.Fatal(err)
	}
	route := r.LastRoute()
	if route.Provider != "anthropic" || route.Fallbacks != 1 ||
		route.CompatKey != "anthropic|m|messages" {
		t.Fatalf("unexpected route record: %+v", route)
	}
	r2, _ := NewRouterProvider(keyedProvider{name: "echo", text: "x", key: "echo|std"})
	if _, _, err = r2.Complete(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if route = r2.LastRoute(); route.Fallbacks != 0 || route.Provider != "echo" {
		t.Fatalf("first-choice route wrong: %+v", route)
	}
	r3, _ := NewRouterProvider(failingProvider{"a"}, failingProvider{"b"})
	if _, _, err = r3.Complete(context.Background(), "p"); err == nil {
		t.Fatal("expected failure")
	}
	if route = r3.LastRoute(); route.Provider != "" {
		t.Fatalf("failed route must be empty: %+v", route)
	}
}
