package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	maxConnections    = 32
	maxPendingPrompts = 16
	outboundQueueSize = 256
	writeTimeout      = 10 * time.Second
)

// Serve takes ownership of a supervisor-created Unix or vsock listener. Each connection
// gets a fresh provider and Engine; IDs confer no authority across connections.
// Providers must honor cancellation, including during shutdown.
func Serve(ctx context.Context, listener net.Listener, newProvider func() Provider) error {
	if network := listener.Addr().Network(); network != "unix" && network != "vsock" {
		return fmt.Errorf("engine requires a Unix or vsock stream listener")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { listener.Close() })
	defer stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	// Cancel before waiting for active connections if Accept fails.
	defer cancel()
	slots := make(chan struct{}, maxConnections)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-slots }()
				var provider Provider
				if newProvider != nil {
					provider = newProvider()
				}
				serveConnection(ctx, conn, provider)
			}()
		default:
			conn.Close()
		}
	}
}

func serveConnection(parent context.Context, conn net.Conn, provider Provider) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	outgoing := make(chan []byte, outboundQueueSize)
	var queueMu sync.Mutex
	queuedBytes := 0
	// Never block with Engine.mu held. Queue overflow terminates this connection;
	// this prototype does not claim suspend/resume or resumable event delivery.
	send := func(m Message) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		data, err := json.Marshal(m)
		if err != nil {
			cancel()
			return
		}
		data = append(data, '\n')
		queueMu.Lock()
		defer queueMu.Unlock()
		if len(data) > maxOutboundBytes-queuedBytes {
			cancel()
			return
		}
		select {
		case outgoing <- data:
			queuedBytes += len(data)
		default:
			cancel()
		}
	}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-outgoing:
				if err := conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
					cancel()
					return
				}
				n, err := conn.Write(m)
				if err == nil && n != len(m) {
					err = io.ErrShortWrite
				}
				queueMu.Lock()
				queuedBytes -= len(m)
				queueMu.Unlock()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	engine := NewEngineWithProvider(provider)
	engine.onEvent = send
	var prompts sync.WaitGroup
	defer func() { cancel(); engine.Close(); prompts.Wait(); <-writerDone }()
	slots := make(chan struct{}, maxPendingPrompts)
	decoder := NewDecoder(conn)
	initialized := false
	reply := func(request Message, response Message) {
		if len(request.ID) != 0 {
			send(response)
		}
	}
	rpcError := func(id json.RawMessage, code int, message string) {
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		send(Message{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: message}})
	}
	for {
		var request Message
		err := decoder.Read(&request)
		if err != nil {
			var rpc *RPCError
			if errors.As(err, &rpc) {
				rpcError(nil, rpc.Code, rpc.Message)
				continue
			}
			if errors.Is(err, ErrInvalidMessage) {
				rpcError(nil, -32600, "invalid request")
				continue
			}
			return // EOF, oversized frame or transport failure.
		}
		if request.Method == "" {
			rpcError(nil, -32600, "expected request")
			continue
		}
		if !initialized && request.Method != "initialize" {
			reply(request, Message{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32002, Message: "initialize required"}})
			continue
		}
		if request.Method == "initialize" {
			// A handshake requires a response; a notification cannot initialize.
			if len(request.ID) == 0 {
				continue
			}
			response := engine.Handle(request)
			var fields map[string]any
			_ = json.Unmarshal(response.Result, &fields)
			fields["address"] = conn.LocalAddr().Network() + "://" + conn.LocalAddr().String()
			response.Result, _ = json.Marshal(fields)
			send(response)
			initialized = true
			continue
		}
		if request.Method != "session/prompt" {
			reply(request, engine.Handle(request))
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			reply(request, Message{JSONRPC: "2.0", ID: request.ID, Error: &RPCError{Code: -32001, Message: "too many pending prompts"}})
			continue
		}
		ready := make(chan struct{})
		var once sync.Once
		prompts.Add(1)
		go func(request Message) {
			defer prompts.Done()
			defer func() { <-slots }()
			response := engine.handle(request, func() { once.Do(func() { close(ready) }) })
			reply(request, response)
		}(request)
		// Preserve wire order: an immediately following abort must observe this turn.
		select {
		case <-ready:
		case <-ctx.Done():
			return
		}
	}
}
