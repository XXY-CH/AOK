//go:build linux && arm64

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"aok/runtime/kernelbridge"
)

// KernelInferProvider runs every supervisor turn through the kernel
// inference ABI (patch 0006): the prompt is submitted as a client session,
// the wrapped provider serves the backend half, and the kernel-checked
// result (usage reconciliation, budget enforcement, revocation) is the only
// accepted outcome. This is the supervisor-side half of the ainf device:
// token budgets and EDQUOT enforcement live in the kernel, not the runtime.
type KernelInferProvider struct {
	root      *kernelbridge.Root
	cap       *kernelbridge.Capability
	inner     Provider
	limit     uint64
	maxOutput uint64

	// mu serializes Complete: the kernel reports a capability-wide ledger
	// at Result time, so concurrent sessions could otherwise observe each
	// other's settles and fail reconciliation spuriously. settled mirrors
	// every charge the kernel makes against the capability, including the
	// full-reservation charges on cancelled or abandoned sessions.
	mu       sync.Mutex
	settled  uint64
	lastTain uint64
}

// LastTaint returns the kernel-reported taint of the most recent session
// result: the kernel ORs the session's policy taint into every record, so
// a nonzero value taints this turn's output dataflow.
func (p *KernelInferProvider) LastTaint() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastTain
}

// NewKernelInferProvider wraps inner with the kernel inference device. The
// root capability must already be claimed (PID1 or handed over via
// AOK_ROOT_FD); tokenLimit is the kernel-enforced budget for the lifetime
// of the derived capability and maxOutputTokens is the per-turn output
// reservation submitted before the inner provider runs.
func NewKernelInferProvider(root *kernelbridge.Root, inner Provider, tokenLimit, maxOutputTokens uint64) (*KernelInferProvider, error) {
	if root == nil || inner == nil {
		return nil, fmt.Errorf("kernel infer requires a root capability and an inner provider")
	}
	// The provider plays both session halves: Infer for the client side,
	// Serve for the backend half that wraps the inner provider.
	capability, err := root.Create(kernelbridge.CapabilitySpec{
		Rights:     kernelbridge.Infer | kernelbridge.Serve,
		TokenLimit: tokenLimit,
	})
	if err != nil {
		return nil, err
	}
	return &KernelInferProvider{root: root, cap: capability, inner: inner,
		limit: tokenLimit, maxOutput: maxOutputTokens}, nil
}

func (p *KernelInferProvider) Name() string { return "kernel-infer/" + p.inner.Name() }

func (p *KernelInferProvider) CompatKey() string {
	// The kernel device is the deployment; the inner provider only serves
	// the backend half inside the session.
	return "ainf|" + compatKeyOf(p.inner)
}

func (p *KernelInferProvider) Close() error { return p.cap.Close() }

func (p *KernelInferProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	if len(prompt) > kernelbridge.DataMax {
		return "", Usage{}, fmt.Errorf("kernel infer prompt exceeds device limit")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	client, backend, err := p.cap.Session()
	if err != nil {
		return "", Usage{}, err
	}
	defer client.Close()
	defer backend.Close()
	// Sequence numbers are per session and this provider uses one submit
	// per session, so every session starts at 1. The kernel takes the
	// reservation at submit time and settles the real usage at completion,
	// so reserve the input plus the declared output ceiling up front.
	reservation := uint64(len(prompt)) + p.maxOutput
	if err := client.Submit(kernelbridge.Record{
		Sequence: 1, InputTokens: uint64(len(prompt)),
		OutputTokens: p.maxOutput, Data: []byte(prompt),
	}); err != nil {
		// A failed submit reserves nothing and charges nothing.
		return "", Usage{}, fmt.Errorf("kernel submit: %w", err)
	}
	request, err := backend.Take()
	if err != nil {
		// A failed take leaves the session PENDING, which releases with a
		// zero charge.
		return "", Usage{}, fmt.Errorf("kernel take: %w", err)
	}
	// From here the session is RUNNING: the kernel charges the full
	// reservation on both cancel and an abandoned (failed) completion.
	text, usage, err := p.inner.Complete(ctx, string(request.Data))
	if err != nil {
		_ = client.Cancel()
		p.settled += reservation
		return "", Usage{}, err
	}
	settled := usage.InputTokens + usage.OutputTokens
	if err := backend.Complete(kernelbridge.Record{
		Sequence: request.Sequence, InputTokens: usage.InputTokens,
		OutputTokens: usage.OutputTokens, Data: []byte(text),
	}); err != nil {
		p.settled += reservation
		return "", Usage{}, fmt.Errorf("kernel complete: %w", err)
	}
	result, err := client.Result()
	if err != nil {
		// Completion already settled the real usage; a failed result read
		// closes a DONE session with no further charge — the ledger must
		// still advance or every later turn would mismatch.
		p.settled += settled
		return "", Usage{}, fmt.Errorf("kernel result: %w", err)
	}
	p.settled += settled
	p.lastTain = result.Taint
	reconciled := Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
	if result.TokensUsed != p.settled || !bytes.Equal(result.Data, []byte(text)) {
		return "", Usage{}, fmt.Errorf("kernel reconciliation mismatch: used=%d expected=%d",
			result.TokensUsed, p.settled)
	}
	return text, reconciled, nil
}
