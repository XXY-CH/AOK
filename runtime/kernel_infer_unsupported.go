//go:build !linux || !arm64

package runtime

import (
	"context"

	"aok/runtime/kernelbridge"
)

// KernelInferProvider is unavailable off the AOK kernel; the supervisor
// falls back to direct providers.
type KernelInferProvider struct{}

func NewKernelInferProvider(any, Provider, uint64) (*KernelInferProvider, error) {
	return nil, kernelbridge.ErrUnsupported
}

func (p *KernelInferProvider) Name() string { return "kernel-infer" }

func (p *KernelInferProvider) Complete(context.Context, string) (string, Usage, error) {
	return "", Usage{}, kernelbridge.ErrUnsupported
}
