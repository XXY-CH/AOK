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
