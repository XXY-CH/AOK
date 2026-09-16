package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// RouterProvider tries supervisor-selected providers in order. The prompt is
// never allowed to select a provider; route order comes from trusted policy.
type RouterProvider struct {
	providers []Provider
}

func NewRouterProvider(providers ...Provider) (*RouterProvider, error) {
	if len(providers) == 0 {
		return nil, errors.New("router requires at least one provider")
	}
	seen := make(map[string]struct{}, len(providers))
	copyProviders := make([]Provider, 0, len(providers))
	for _, provider := range providers {
		if provider == nil || provider.Name() == "" {
			return nil, errors.New("router provider is invalid")
		}
		if _, ok := seen[provider.Name()]; ok {
			return nil, fmt.Errorf("router provider %q is duplicated", provider.Name())
		}
		seen[provider.Name()] = struct{}{}
		copyProviders = append(copyProviders, provider)
	}
	return &RouterProvider{providers: copyProviders}, nil
}

func (r *RouterProvider) Name() string { return "router" }

func (r *RouterProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	var failures []string
	for _, provider := range r.providers {
		if err := ctx.Err(); err != nil {
			return "", Usage{}, err
		}
		text, usage, err := provider.Complete(ctx, prompt)
		if err == nil {
			return text, usage, nil
		}
		if ctx.Err() != nil {
			return "", Usage{}, ctx.Err()
		}
		failures = append(failures, provider.Name()+": "+err.Error())
	}
	return "", Usage{}, errors.New("all routed providers failed: " + strings.Join(failures, "; "))
}
