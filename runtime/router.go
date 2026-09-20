package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// RouteInfo records one routed turn: which provider served it, how many
// earlier candidates failed, and the serving provider's compatibility key.
// It is the runtime side of the route_record contract in docs/abi/route-model.md.
type RouteInfo struct {
	Provider  string `json:"provider"`
	Fallbacks uint32 `json:"fallbacks"`
	CompatKey string `json:"compat_key"`
}

// CompatKeyer lets a provider declare its KV compatibility key
// (model | tokenizer | template | quantization | version | device). KV may
// only be reused across turns whose keys match exactly; any change forces
// the recovery level to degrade per the route model.
type CompatKeyer interface {
	CompatKey() string
}

// RouterProvider tries supervisor-selected providers in order. The prompt is
// never allowed to select a provider; route order comes from trusted policy.
type RouterProvider struct {
	mu        sync.Mutex
	providers []Provider
	last      RouteInfo
}

// LastRoute returns the routing decision of the most recent Complete call.
func (r *RouterProvider) LastRoute() RouteInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
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

type routeBackendsKey struct{}

// WithRouteBackends attaches an application's allowed backend fallback
// order to the turn context. An empty or absent list means unrestricted.
func WithRouteBackends(ctx context.Context, backends []string) context.Context {
	if len(backends) == 0 {
		return ctx
	}
	return context.WithValue(ctx, routeBackendsKey{}, backends)
}

func (r *RouterProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	text, usage, _, _, err := r.completeRouted(ctx, prompt)
	return text, usage, err
}

// CompleteTracked returns the routing decision with the turn it belongs to:
// under parallel execution the latest-wins LastRoute getter would attribute
// another turn's route to this one.
func (r *RouterProvider) CompleteTracked(ctx context.Context, prompt string) (string, Usage, RouteInfo, uint64, error) {
	return r.completeRouted(ctx, prompt)
}

func (r *RouterProvider) completeRouted(ctx context.Context, prompt string) (string, Usage, RouteInfo, uint64, error) {
	var failures []string
	allowed, _ := ctx.Value(routeBackendsKey{}).([]string)
	candidates := r.providers
	if len(allowed) > 0 {
		candidates = nil
		seen := map[string]bool{}
		for _, name := range allowed {
			for _, provider := range r.providers {
				if provider.Name() == name && !seen[name] {
					candidates = append(candidates, provider)
					seen[name] = true
				}
			}
		}
	}
	for _, provider := range candidates {
		if err := ctx.Err(); err != nil {
			return "", Usage{}, RouteInfo{}, 0, err
		}
		text, usage, err := provider.Complete(ctx, prompt)
		if err == nil {
			route := RouteInfo{Provider: provider.Name(),
				// Policy-skipped providers are not attempts; only real
				// failures count as fallbacks.
				Fallbacks: uint32(len(failures)), CompatKey: compatKeyOf(provider)}
			r.mu.Lock()
			r.last = route
			r.mu.Unlock()
			return text, usage, route, 0, nil
		}
		if ctx.Err() != nil {
			return "", Usage{}, RouteInfo{}, 0, ctx.Err()
		}
		failures = append(failures, provider.Name()+": "+err.Error())
	}
	r.mu.Lock()
	r.last = RouteInfo{}
	r.mu.Unlock()
	if len(failures) == 0 {
		return "", Usage{}, RouteInfo{}, 0, errors.New("route policy excludes every backend")
	}
	return "", Usage{}, RouteInfo{}, 0, errors.New("all routed providers failed: " + strings.Join(failures, "; "))
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

func compatKeyOf(provider Provider) string {
	if keyed, ok := provider.(CompatKeyer); ok {
		return keyed.CompatKey()
	}
	return provider.Name()
}
