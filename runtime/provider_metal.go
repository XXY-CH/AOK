package runtime

import (
	"context"
	"errors"
	"net/url"
	"strings"
)

// MetalProvider is the supervisor-owned local llama.cpp service configured to
// use Apple's Metal device. The device selection remains in the backend
// process; this adapter exposes a distinct policy identity to the router.
type MetalProvider struct{ llama *LlamaProvider }

type MetalConfig struct {
	Endpoint         string
	MaxTokens        int
	MaxResponseBytes int64
}

func NewMetalProvider(config MetalConfig) (*MetalProvider, error) {
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("metal endpoint must be an HTTP(S) base URL")
	}
	if !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("metal endpoint must be loopback")
	}
	llama, err := NewLlamaProvider(LlamaConfig{Endpoint: strings.TrimRight(config.Endpoint, "/"), MaxTokens: config.MaxTokens, MaxResponseBytes: config.MaxResponseBytes, CachePrompt: true})
	if err != nil {
		return nil, err
	}
	return &MetalProvider{llama: llama}, nil
}

func (p *MetalProvider) Name() string { return "metal" }

func (p *MetalProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	if p == nil || p.llama == nil {
		return "", Usage{}, errors.New("metal provider is unavailable")
	}
	return p.llama.Complete(ctx, prompt)
}
