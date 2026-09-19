package runtime

import "context"

type Usage struct {
	InputTokens  uint64 `json:"input_tokens"`
	OutputTokens uint64 `json:"output_tokens"`
	CachedTokens uint64 `json:"cached_tokens"`
}

type Provider interface {
	Name() string
	Complete(context.Context, string) (text string, usage Usage, err error)
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
