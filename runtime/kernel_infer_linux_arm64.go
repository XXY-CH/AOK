//go:build linux && arm64

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"

	"aok/runtime/kernelbridge"
)

// openKernelInferRoot returns the boot root capability for the inference
// device. The claim is one-shot and owned by PID1, so the production
// supervisor only accepts a descriptor handed over via AOK_ROOT_FD; a
// derived probe capability proves the descriptor before use.
func openKernelInferRoot() (*kernelbridge.Root, error) {
	fd := os.Getenv("AOK_ROOT_FD")
	if fd == "" {
		return nil, fmt.Errorf("kernel infer requires AOK_ROOT_FD from PID1")
	}
	number, err := strconv.Atoi(fd)
	if err != nil || number < 0 {
		return nil, kernelbridge.ErrUnsupported
	}
	// The event bridge wraps the same descriptor; dup before wrapping so a
	// dropped wrapper's finalizer cannot close the shared fd underneath the
	// other user.
	dup, err := unix.Dup(number)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(dup), "aok-root")
	if file == nil {
		return nil, kernelbridge.ErrUnsupported
	}
	root := kernelbridge.RootFromFile(file)
	probe, err := root.Create(kernelbridge.CapabilitySpec{Rights: kernelbridge.Infer, TokenLimit: 1})
	if err != nil {
		return nil, err
	}
	_ = probe.Close()
	return root, nil
}

// WrapKernelInferProvider wraps a production provider with the kernel
// inference device: every turn runs through a kernel session with kernel
// reservation, settlement and revocation. Off the AOK kernel it fails
// loudly instead of silently bypassing the device.
func WrapKernelInferProvider(inner Provider, tokenLimit, maxOutputTokens uint64) (Provider, error) {
	root, err := openKernelInferRoot()
	if err != nil {
		return nil, err
	}
	return NewKernelInferProvider(root, inner, tokenLimit, maxOutputTokens)
}

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
	// slot is the single-session admission: the kernel capability's token
	// ledger reconciles against this provider's mirror, so only one session
	// may be in flight. A second concurrent turn returns ErrTurnBusy and
	// the runner requeues it instead of burning its deadline on a mutex.
	slot chan struct{}

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
	p := &KernelInferProvider{root: root, cap: capability, inner: inner,
		limit: tokenLimit, maxOutput: maxOutputTokens, slot: make(chan struct{}, 1)}
	p.slot <- struct{}{}
	return p, nil
}

func (p *KernelInferProvider) Name() string { return "kernel-infer/" + p.inner.Name() }

func (p *KernelInferProvider) CompatKey() string {
	// The kernel device is the deployment; the inner provider only serves
	// the backend half inside the session.
	return "ainf|" + compatKeyOf(p.inner)
}

func (p *KernelInferProvider) Close() error { return p.cap.Close() }

func (p *KernelInferProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	text, usage, _, _, err := p.runSession(ctx, prompt)
	return text, usage, err
}

// CompleteTracked returns the serving route and the session taint with the
// turn that produced them; the inner provider may itself be tracked (a
// router), in which case its decision is the serving route.
func (p *KernelInferProvider) CompleteTracked(ctx context.Context, prompt string) (string, Usage, RouteInfo, uint64, error) {
	return p.runSession(ctx, prompt)
}

func (p *KernelInferProvider) runSession(ctx context.Context, prompt string) (string, Usage, RouteInfo, uint64, error) {
	if len(prompt) > kernelbridge.DataMax {
		return "", Usage{}, RouteInfo{}, 0, fmt.Errorf("kernel infer prompt exceeds device limit")
	}
	// Admission before the session: a queued turn never starts its kernel
	// session, charges nothing, and reports busy instead of failing.
	select {
	case <-p.slot:
	default:
		return "", Usage{}, RouteInfo{}, 0, ErrTurnBusy
	}
	defer func() { p.slot <- struct{}{} }()
	p.mu.Lock()
	defer p.mu.Unlock()
	client, backend, err := p.cap.Session()
	if err != nil {
		return "", Usage{}, RouteInfo{}, 0, err
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
		return "", Usage{}, RouteInfo{}, 0, fmt.Errorf("kernel submit: %w", err)
	}
	request, err := backend.Take()
	if err != nil {
		// A failed take leaves the session PENDING, which releases with a
		// zero charge.
		return "", Usage{}, RouteInfo{}, 0, fmt.Errorf("kernel take: %w", err)
	}
	// From here the session is RUNNING: the kernel charges the full
	// reservation on both cancel and an abandoned (failed) completion.
	route := RouteInfo{Provider: p.Name(), CompatKey: p.CompatKey()}
	var taint uint64
	var text string
	var usage Usage
	if tracked, ok := p.inner.(TrackedProvider); ok {
		text, usage, route, taint, err = tracked.CompleteTracked(ctx, string(request.Data))
	} else {
		text, usage, err = p.inner.Complete(ctx, string(request.Data))
	}
	if err != nil {
		_ = client.Cancel()
		p.settled += reservation
		return "", Usage{}, RouteInfo{}, 0, err
	}
	settled := usage.InputTokens + usage.OutputTokens
	if err := backend.Complete(kernelbridge.Record{
		Sequence: request.Sequence, InputTokens: usage.InputTokens,
		OutputTokens: usage.OutputTokens, Data: []byte(text),
	}); err != nil {
		p.settled += reservation
		return "", Usage{}, RouteInfo{}, 0, fmt.Errorf("kernel complete: %w", err)
	}
	result, err := client.Result()
	if err != nil {
		// Completion already settled the real usage; a failed result read
		// closes a DONE session with no further charge — the ledger must
		// still advance or every later turn would mismatch.
		p.settled += settled
		return "", Usage{}, RouteInfo{}, 0, fmt.Errorf("kernel result: %w", err)
	}
	p.settled += settled
	p.lastTain = result.Taint
	reconciled := Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
	if result.TokensUsed != p.settled || !bytes.Equal(result.Data, []byte(text)) {
		return "", Usage{}, RouteInfo{}, 0, fmt.Errorf("kernel reconciliation mismatch: used=%d expected=%d",
			result.TokensUsed, p.settled)
	}
	return text, reconciled, route, result.Taint | taint, nil
}
