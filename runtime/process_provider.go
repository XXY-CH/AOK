package runtime

import (
	"aok/runtime/sandbox"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

var ErrRestartIntensity = errors.New("engine restart intensity exceeded")

type ProcessProviderConfig struct {
	Command            string
	Args               []string
	StartupTimeout     time.Duration
	RequestTimeout     time.Duration
	ShutdownTimeout    time.Duration
	MaxStarts          int
	ResourceController interface{ AttachPID(int) error }
	SandboxLauncher    string
	SandboxPolicy      sandbox.Policy
}
type processChild struct {
	dir      string
	cmd      *exec.Cmd
	done     chan struct{}
	listener *net.UnixListener
}
type ProcessProvider struct {
	mu     sync.Mutex
	config ProcessProviderConfig
	name   string
	starts []time.Time
	child  *processChild
	closed bool
}

func NewProcessProvider(c ProcessProviderConfig) (*ProcessProvider, error) {
	if c.Command == "" || !filepath.IsAbs(c.Command) {
		return nil, errors.New("engine command must be absolute")
	}
	if c.StartupTimeout == 0 {
		c.StartupTimeout = 5 * time.Second
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 2 * time.Minute
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = time.Second
	}
	if c.MaxStarts == 0 {
		c.MaxStarts = 5
	}
	if c.StartupTimeout < 0 || c.RequestTimeout <= 0 || c.ShutdownTimeout < 0 || c.MaxStarts < 1 {
		return nil, errors.New("invalid process provider limits")
	}
	return &ProcessProvider{config: c, name: filepath.Base(c.Command)}, nil
}
func (p *ProcessProvider) Name() string { return p.name }

func (p *ProcessProvider) reapLocked(ch *processChild) {
	if p.child != ch {
		return
	}
	select {
	case <-ch.done:
	default:
		_ = ch.cmd.Process.Kill()
		<-ch.done
	}
	p.child = nil
	_ = ch.listener.Close()
	_ = os.RemoveAll(ch.dir)
}
func (p *ProcessProvider) startLocked(ctx context.Context) error {
	if p.closed {
		return errors.New("engine process closed")
	}
	if p.child != nil {
		return nil
	}
	now := time.Now()
	kept := p.starts[:0]
	for _, t := range p.starts {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	p.starts = kept
	if len(p.starts) >= p.config.MaxStarts {
		return ErrRestartIntensity
	}
	p.starts = append(p.starts, now)
	dir, err := os.MkdirTemp("", "aok-engine-")
	if err != nil {
		return err
	}
	// Abstract Unix sockets remain addressable after the parent closes its copy;
	// this avoids treating the parent's unused listener as child readiness.
	addr := "@aok-" + filepath.Base(dir)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: addr, Net: "unix"})
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	lf, err := l.File()
	if err != nil {
		l.Close()
		os.RemoveAll(dir)
		return err
	}
	args := append([]string(nil), p.config.Args...)
	for i, arg := range args {
		if arg == "--aok-child" {
			args = append(args[:i], append([]string{"--"}, args[i:]...)...)
			break
		}
	}
	command := p.config.Command
	if p.config.SandboxLauncher != "" {
		pol := p.config.SandboxPolicy
		pol.KeepFD = []int{3}
		args, err = pol.Args(append([]string{command}, args...))
		if err != nil {
			lf.Close()
			l.Close()
			os.RemoveAll(dir)
			return err
		}
		command = p.config.SandboxLauncher
	}
	cmd := exec.Command(command, args...)
	cmd.ExtraFiles = []*os.File{lf}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GOMAXPROCS=2"}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		lf.Close()
		l.Close()
		os.RemoveAll(dir)
		return err
	}
	if p.config.ResourceController != nil {
		if err = p.config.ResourceController.AttachPID(cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			return err
		}
	}
	lf.Close()
	done := make(chan struct{})
	ch := &processChild{dir: dir, cmd: cmd, done: done, listener: l}
	p.child = ch
	go func() { _ = cmd.Wait(); close(done) }()
	_ = l.Close()
	timer := time.NewTimer(p.config.StartupTimeout)
	defer timer.Stop()
	for {
		c, e := (&net.Dialer{}).DialContext(ctx, "unix", addr)
		if e == nil {
			_ = c.SetDeadline(time.Now().Add(50 * time.Millisecond))
			id := json.RawMessage("1")
			if writeErr := NewEncoder(c).Write(Message{JSONRPC: "2.0", ID: id, Method: "initialize", Params: json.RawMessage("{}")}); writeErr == nil {
				var response Message
				if readErr := NewDecoder(c).Read(&response); readErr == nil && string(response.ID) == string(id) && response.Error == nil {
					c.Close()
					return nil
				}
			}
			c.Close()
		}
		select {
		case <-done:
			p.reapLocked(ch)
			return errors.New("engine exited during startup")
		case <-ctx.Done():
			p.reapLocked(ch)
			return ctx.Err()
		case <-timer.C:
			p.reapLocked(ch)
			return errors.New("engine startup timeout")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
func (p *ProcessProvider) stopLocked() {
	if p.child != nil {
		p.reapLocked(p.child)
	}
}
func (p *ProcessProvider) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.stopLocked()
	return nil
}

func (p *ProcessProvider) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(prompt) > maxTextBytes {
		return "", Usage{}, errors.New("prompt exceeds engine limit")
	}
	if err := ctx.Err(); err != nil {
		return "", Usage{}, err
	}
	if err := p.startLocked(ctx); err != nil {
		return "", Usage{}, err
	}
	ch := p.child
	timeout := p.config.RequestTimeout
	if d, ok := ctx.Deadline(); ok && time.Until(d) < timeout {
		timeout = time.Until(d)
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := "@aok-" + filepath.Base(ch.dir)
	conn, err := (&net.Dialer{}).DialContext(rctx, "unix", addr)
	if err != nil {
		p.reapLocked(ch)
		return "", Usage{}, err
	}
	defer conn.Close()
	context.AfterFunc(rctx, func() { conn.Close() })
	enc, dec := NewEncoder(conn), NewDecoder(conn)
	var text string
	var usage Usage
	call := func(id int, method string, params any, result any) error {
		data, _ := json.Marshal(params)
		idj, _ := json.Marshal(id)
		if err := enc.Write(Message{JSONRPC: "2.0", ID: idj, Method: method, Params: data}); err != nil {
			return err
		}
		for {
			var m Message
			if err := dec.Read(&m); err != nil {
				return err
			}
			if m.Method != "" {
				if m.Method == "session/chunk" {
					var v struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(m.Params, &v) != nil {
						return errors.New("invalid engine chunk")
					}
					text += v.Text
				}
				if m.Method == "session/usage" {
					var v struct {
						Usage Usage `json:"usage"`
					}
					if json.Unmarshal(m.Params, &v) != nil {
						return errors.New("invalid engine usage")
					}
					usage = v.Usage
				}
				continue
			}
			if string(m.ID) != string(idj) {
				return errors.New("engine response ID mismatch")
			}
			if m.Error != nil {
				return fmt.Errorf("engine request rejected: %s", m.Error.Message)
			}
			if result != nil {
				return json.Unmarshal(m.Result, result)
			}
			return nil
		}
	}
	var session struct {
		SessionID string `json:"session_id"`
	}
	if err = call(1, "initialize", struct{}{}, nil); err == nil {
		err = call(2, "session/new", struct{}{}, &session)
	}
	if err == nil && session.SessionID == "" {
		err = errors.New("empty engine session")
	}
	if err == nil {
		var done struct {
			StopReason string `json:"stop_reason"`
		}
		err = call(3, "session/prompt", map[string]string{"session_id": session.SessionID, "request_id": "turn-1", "text": prompt}, &done)
		if err == nil && done.StopReason != "completed" {
			err = errors.New("engine turn did not complete")
		}
	}
	if err != nil {
		p.reapLocked(ch)
		if rctx.Err() != nil {
			return "", usage, rctx.Err()
		}
		return "", usage, err
	}
	return text, usage, nil
}
