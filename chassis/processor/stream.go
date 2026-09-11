package processor

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// streamSink is the per-request bridge an OP uses to push response-body
// bytes to the client WHILE IT IS STILL RUNNING — the missing half of the
// streaming response path.
//
// The chassis already streams scope-driven bodies: a `_txc.web.res.body`
// written in a non-terminal scope is flushed as a chunk by
// advanceAfterScope, which owns the StreamHead/StreamChunk/StreamEnd
// sequence the web outlet renders. That granularity is one chunk per scope
// hop. An op that produces output continuously — a `workspace://` exec
// running a build — has nothing to write to between hops, because an op
// handler returns exactly ONE payload when it is done.
//
// The sink closes that gap. It is installed on the request context by Run
// (live HTTP requests only) holding the same response channel
// advanceAfterScope uses, so an op can emit chunks in the middle of its own
// dispatch and the outlet writes them as they arrive. advanceAfterScope
// then treats an opened sink exactly like the envelope's
// `_txc.runtime.http_stream_open` flag: the terminal scope closes the
// stream instead of emitting a buffered JSON response.
//
// Ordering and backpressure: sends are serialized under mu (so the head
// can never be overtaken by a chunk) and block until the outlet receives,
// which is the natural backpressure — resCh is unbuffered, so a command
// producing faster than the client reads is throttled at the source. Every
// send selects on the op's context, so a client that disappears (the outlet
// cancels) unblocks the writer instead of wedging it.
//
// Once a stream is open the response IS the stream: the envelope's JSON
// rendering no longer reaches the client, because the status and headers
// went out with the first chunk. That is HTTP, not a chassis choice.
type streamSink struct {
	resCh chan event.Payload

	mu     sync.Mutex // serializes sends: head strictly before its chunk
	opened atomic.Bool
	closed atomic.Bool
	bytes  atomic.Int64
}

// ctxKeyStreamSink carries the per-request sink. Absent for every run that
// has no live client (resume, deferred join, async capture) — op code must
// treat a nil sink as "streaming is not available here".
var ctxKeyStreamSink = ctxKeyType{name: "stream-sink"}

func withStreamSink(ctx context.Context, s *streamSink) context.Context {
	return context.WithValue(ctx, ctxKeyStreamSink, s)
}

// streamSinkFrom returns the request's sink, or nil when this run cannot
// stream.
func streamSinkFrom(ctx context.Context) *streamSink {
	s, _ := ctx.Value(ctxKeyStreamSink).(*streamSink)
	return s
}

// Opened reports whether the head has gone out — i.e. whether the client is
// already receiving a streamed response. Never blocks (advanceAfterScope
// consults it on the merge path).
func (s *streamSink) Opened() bool { return s.opened.Load() }

// Bytes is how many body bytes have been streamed.
func (s *streamSink) Bytes() int64 { return s.bytes.Load() }

// markClosed records that the terminating StreamEnd has been emitted (by
// advanceAfterScope, which owns the response channel's lifecycle). Later
// writes become no-ops rather than racing bytes onto a finished response.
func (s *streamSink) markClosed() { s.closed.Store(true) }

// Write sends p as a body chunk, emitting head as the StreamHead first if
// this is the first chunk of the response. head is an envelope carrying the
// `_txc.web.res.*` status and headers snapshot the outlet renders; it is
// used only on that first call. Empty writes are dropped so a command that
// produces nothing never opens a stream (the request then renders its
// normal JSON response).
func (s *streamSink) Write(ctx context.Context, head string, p []byte) error {
	if len(p) == 0 || s.closed.Load() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil
	}
	if !s.opened.Load() {
		if err := s.send(ctx, event.Payload{Raw: head, Type: event.StreamHead}); err != nil {
			return err
		}
		s.opened.Store(true)
	}
	if err := s.send(ctx, event.Payload{Raw: string(p), Type: event.StreamChunk}); err != nil {
		return err
	}
	s.bytes.Add(int64(len(p)))
	return nil
}

// send blocks until the outlet takes the payload or the op's context ends.
func (s *streamSink) send(ctx context.Context, p event.Payload) error {
	select {
	case s.resCh <- p:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
