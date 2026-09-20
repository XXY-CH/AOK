package runtime

import (
	"context"
	"errors"
)

type Usage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	CachedTokens uint64 `json:"cached_tokens"`
}

type Provider interface {
	Name() string
	Complete(context.Context, string) (text string, usage Usage, err error)
}

// ErrTurnBusy marks a turn that was not attempted because the provider's
// single execution slot was occupied. The runner requeues the message
// without counting a failure: the engine never saw the prompt.
var ErrTurnBusy = errors.New("provider busy, turn not attempted")

// TrackedProvider carries the per-turn routing decision and kernel taint
// with the call that produced them. Concurrent turns share one provider, so
// post-hoc getters (LastRoute/LastTaint) can only report the most recent
// turn — under parallel execution they misattribute. Providers that track
// per-turn state implement this instead.
type TrackedProvider interface {
	CompleteTracked(ctx context.Context, prompt string) (text string, usage Usage, route RouteInfo, taint uint64, err error)
}

// EchoProvider is deterministic and offline; it is used for protocol tests and
// can be replaced by a llama.cpp or cloud adapter without changing Engine.
type EchoProvider struct{}

func (EchoProvider) Name() string { return "echo" }

func (EchoProvider) CompatKey() string { return "echo|std|plain|f32|1|cpu" }
func (EchoProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	select {
	case <-ctx.Done():
		return "", Usage{}, ctx.Err()
	default:
	}
	return prompt, Usage{InputTokens: uint64(len(prompt)), OutputTokens: uint64(len(prompt))}, nil
}
