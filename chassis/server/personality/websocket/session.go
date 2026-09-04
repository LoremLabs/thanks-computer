package websocket

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

const (
	initiatorClient  = "client"
	initiatorStack   = "stack"
	initiatorChassis = "chassis"
)

// conn is the slice of the WebSocket library a session uses. *websocket.Conn
// satisfies it; tests substitute a fake.
type conn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Ping(ctx context.Context) error
	Close(code websocket.StatusCode, reason string) error
	CloseNow() error
	Subprotocol() string
}

type inboundMsg struct {
	typ  MessageType
	data []byte
}

// session is one accepted connection. Per-connection state lives here and
// is stamped onto every run's envelope; nothing on the socket is ever
// handed to a stack.
type session struct {
	c    *Controller
	id   string
	acc  Accept
	conn conn

	tenant, stack string
	clientIP      string
	host, path    string
	origin, ua    string
	subprotocol   string
	connectedAt   time.Time

	ctx    context.Context
	cancel context.CancelFunc

	inbound chan inboundMsg
	done    chan struct{}

	seq          atomic.Int64
	lastActivity atomic.Int64 // unix nanos
	msgsIn       atomic.Int64
	msgsOut      atomic.Int64
	bytesIn      atomic.Int64
	bytesOut     atomic.Int64

	closeOnce sync.Once
	closed    atomic.Bool
}

func newSession(c *Controller, sid string, a Accept, wc conn, r *http.Request) *session {
	ctx, cancel := context.WithCancel(c.ctx)
	now := c.now()
	s := &session{
		c:           c,
		id:          sid,
		acc:         a,
		conn:        wc,
		tenant:      a.Tenant,
		stack:       a.Stack,
		clientIP:    clientIP(r),
		host:        r.Host,
		path:        r.URL.Path,
		origin:      r.Header.Get("Origin"),
		ua:          r.Header.Get("User-Agent"),
		subprotocol: wc.Subprotocol(),
		connectedAt: now,
		ctx:         ctx,
		cancel:      cancel,
		inbound:     make(chan inboundMsg, c.lim.inboundQueue),
		done:        make(chan struct{}),
	}
	s.lastActivity.Store(now.UnixNano())
	return s
}

func (s *session) touch() { s.lastActivity.Store(s.c.now().UnixNano()) }

// reader turns complete messages into queue entries. It never blocks on
// the queue: a full queue closes the session (1013) rather than growing
// memory or starving control frames.
func (s *session) reader() {
	defer s.c.wg.Done()
	for {
		typ, data, err := s.conn.Read(s.ctx)
		if err != nil {
			if s.closed.Load() {
				return // we initiated the close; finish already ran
			}
			code, reason := closeStatusOf(err)
			s.finish(code, reason, initiatorClient, func() { _ = s.conn.CloseNow() })
			return
		}
		mt := MessageText
		if typ == websocket.MessageBinary {
			mt = MessageBinary
		}
		s.bytesIn.Add(int64(len(data)))
		select {
		case s.inbound <- inboundMsg{typ: mt, data: data}:
		default:
			s.c.record(s.c.messages, direction("in"), outcome("dropped"))
			s.closeWith(1013, "inbound queue full", initiatorChassis)
			return
		}
	}
}

// worker runs messages strictly in order: one bounded processor run each.
func (s *session) worker() {
	defer s.c.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case m := <-s.inbound:
			s.handle(m)
		}
	}
}

// pinger owns liveness: a ping every interval (the pong must arrive
// within the next interval), and the idle close.
func (s *session) pinger() {
	defer s.c.wg.Done()
	t := time.NewTicker(s.c.lim.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			idle := time.Duration(s.c.now().UnixNano() - s.lastActivity.Load())
			if idle >= s.acc.IdleTimeout {
				s.closeWith(1000, "idle timeout", initiatorChassis)
				return
			}
			pctx, cancel := context.WithTimeout(s.ctx, s.c.lim.pingInterval)
			err := s.conn.Ping(pctx)
			cancel()
			if err != nil {
				if s.closed.Load() {
					return
				}
				s.closeWith(1011, "ping timeout", initiatorChassis)
				return
			}
		}
	}
}

// handle makes the one run for an inbound message and reads the verdicts
// the chassis cares about off the result: admission, and nothing else — a
// stack talks back with txco://websocket/reply, not with the envelope.
func (s *session) handle(m inboundMsg) {
	s.touch()
	s.msgsIn.Add(1)
	seq := s.seq.Add(1)
	rid := hxid.NewTimeSort().String()
	payload := buildEnvelope(s.envelopeInput(rid, phaseMessage, seq, &m, nil))

	res, ok := s.dispatch(rid, payload, s.c.lim.runTimeout)
	if !ok {
		return
	}
	if status, reason, denied := admission.Denied(res.Raw); denied {
		switch {
		case status == http.StatusTooManyRequests:
			// A token bucket or a concurrency cap: this message is lost, the
			// session is not — a chatty but legitimate client keeps its
			// socket.
			s.c.record(s.c.messages, direction("in"), outcome("rate_limited"))
			s.c.pu.Logger.Info("websocket message rate limited",
				zap.String("sid", s.id), zap.String("rid", rid), zap.String("reason", reason))
		case status == http.StatusServiceUnavailable:
			s.c.record(s.c.messages, direction("in"), outcome("denied"))
			s.closeWith(1013, "service unavailable: "+reason, initiatorChassis)
		default:
			s.c.record(s.c.messages, direction("in"), outcome("denied"))
			s.closeWith(1008, "admission denied: "+reason, initiatorChassis)
		}
		return
	}
	s.c.record(s.c.messages, direction("in"), outcome("ok"))
}

type closeInfo struct {
	code      int
	reason    string
	initiator string
}

func (s *session) envelopeInput(rid, phase string, seq int64, m *inboundMsg, ci *closeInfo) envelopeInput {
	in := envelopeInput{
		rid:              rid,
		now:              s.c.now(),
		tenant:           s.tenant,
		stack:            s.stack,
		ingress:          s.acc.Ingress,
		hostnameVerified: s.acc.HostnameVerified,
		private:          s.c.pu.Conf.DebugPrivate,
		sessionID:        s.id,
		subprotocol:      s.subprotocol,
		connectedAt:      s.connectedAt,
		seq:              seq,
		state:            s.acc.State,
		host:             s.host,
		path:             s.path,
		origin:           s.origin,
		userAgent:        s.ua,
		clientIP:         s.clientIP,
		phase:            phase,
	}
	if m != nil {
		in.msgType, in.msg = m.typ, m.data
	}
	if ci != nil {
		in.closeCode, in.closeReason, in.closeInitiator = ci.code, ci.reason, ci.initiator
	}
	return in
}

// dispatch is the bounded run: LMTP's shape. The run context roots on the
// controller (the request context died with the upgrade handler); the
// response channel is buffered and always drained so an abandoned run can
// never leak the processor goroutine; a streamed result is drained to its
// end — websocket runs do not stream `@web.res.body`.
func (s *session) dispatch(rid, payload string, timeout time.Duration) (event.Payload, bool) {
	if s.c.ctx.Err() != nil {
		return event.Payload{}, false // shutting down: the bus is closing
	}
	ctx, cancel := context.WithTimeout(s.c.ctx, timeout)
	defer cancel()
	ctx = context.WithValue(ctx, config.CtxKeyRid, rid)

	resCh := make(chan event.Payload, 1)
	env := event.PackageJSON(ctx, payload, resCh, Source)
	select {
	case s.c.pu.Bus <- env:
	case <-ctx.Done():
		s.c.record(s.c.messages, direction("in"), outcome("bus_timeout"))
		s.c.pu.Logger.Warn("websocket dispatch: bus timeout", zap.String("sid", s.id), zap.String("rid", rid))
		return event.Payload{}, false
	}
	streamed := false
	for {
		select {
		case res := <-resCh:
			switch res.Type {
			case event.StreamHead, event.StreamChunk:
				if !streamed {
					streamed = true
					s.c.pu.Logger.Debug("websocket run streamed @web.res.body; ignored — use txco://websocket/reply",
						zap.String("sid", s.id), zap.String("rid", rid))
				}
				continue
			case event.StreamEnd:
				return event.Payload{Raw: `{}`, Type: event.JSON}, true
			}
			return res, true
		case <-ctx.Done():
			// The processor's bare send lands in the buffer whenever it
			// finishes; nothing waits on it.
			s.c.record(s.c.messages, direction("in"), outcome("timeout"))
			s.c.pu.Logger.Info("websocket run timeout", zap.String("sid", s.id), zap.String("rid", rid),
				zap.Duration("timeout", timeout))
			return event.Payload{}, false
		}
	}
}

// send writes one message with the write deadline; a failed write closes
// the session — the peer is not reading, and silently dropping application
// messages is the wrong default.
func (s *session) send(ctx context.Context, typ MessageType, data []byte) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}
	if int64(len(data)) > s.acc.MaxMessageBytes {
		return ErrMessageTooLarge
	}
	if ctx == nil {
		ctx = context.Background()
	}
	wctx, cancel := context.WithTimeout(ctx, s.c.lim.writeTimeout)
	defer cancel()
	wt := websocket.MessageText
	if typ == MessageBinary {
		wt = websocket.MessageBinary
	}
	if err := s.conn.Write(wctx, wt, data); err != nil {
		if s.closed.Load() {
			return ErrSessionClosed
		}
		s.c.record(s.c.messages, direction("out"), outcome("write_failed"))
		go s.closeWith(1011, "write failed", initiatorChassis)
		if errors.Is(err, context.DeadlineExceeded) {
			return ErrWriteTimeout
		}
		return ErrSessionClosed
	}
	s.touch()
	s.msgsOut.Add(1)
	s.bytesOut.Add(int64(len(data)))
	s.c.record(s.c.messages, direction("out"), outcome("ok"))
	return nil
}

// closeWith starts a graceful close from our side.
func (s *session) closeWith(code int, reason, who string) {
	s.finish(code, reason, who, func() {
		_ = s.conn.Close(websocket.StatusCode(code), reason)
	})
}

// finish runs exactly once per session: the close (graceful or not), then
// teardown, the log line, and the opt-in close run.
func (s *session) finish(code int, reason, who string, closeFn func()) {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		closeFn()
		close(s.done)
		s.cancel()
		s.c.unregister(s)
		s.c.record(s.c.closes, initiator(who))
		s.c.pu.Logger.Info("websocket close",
			zap.String("tenant", s.tenant), zap.String("stack", s.stack),
			zap.String("sid", s.id), zap.Int("code", code), zap.String("reason", reason),
			zap.String("by", who),
			zap.Int64("in", s.msgsIn.Load()), zap.Int64("out", s.msgsOut.Load()),
			zap.Int64("bytes_in", s.bytesIn.Load()), zap.Int64("bytes_out", s.bytesOut.Load()),
			zap.Duration("duration", s.c.now().Sub(s.connectedAt)))
		if s.acc.Events[EventClose] && s.c.ctx.Err() == nil {
			s.c.wg.Add(1)
			go func() {
				defer s.c.wg.Done()
				rid := hxid.NewTimeSort().String()
				payload := buildEnvelope(s.envelopeInput(rid, phaseClose, s.seq.Add(1), nil,
					&closeInfo{code: code, reason: reason, initiator: who}))
				if res, ok := s.dispatch(rid, payload, s.c.lim.runTimeout); ok {
					if _, r, denied := admission.Denied(res.Raw); denied {
						s.c.pu.Logger.Debug("websocket close run denied", zap.String("sid", s.id), zap.String("reason", r))
					}
				}
			}()
		}
	})
}

// closeStatusOf maps a read error to the close code and reason we record.
func closeStatusOf(err error) (int, string) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return int(ce.Code), ce.Reason
	}
	if errors.Is(err, context.Canceled) {
		return 1001, "connection context cancelled"
	}
	return 1006, truncate(err.Error(), 123)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// --- Registry ---------------------------------------------------------------

// Send implements Registry.
func (c *Controller) Send(ctx context.Context, tenant, id string, typ MessageType, data []byte) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	s, ok := c.lookup(tenant, id)
	if !ok {
		return ErrSessionNotFound
	}
	return s.send(ctx, typ, data)
}

// CloseSession implements Registry. The close handshake runs off the
// caller's goroutine; the op returns as soon as the close is underway.
func (c *Controller) CloseSession(ctx context.Context, tenant, id string, code int, reason string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	s, ok := c.lookup(tenant, id)
	if !ok {
		return ErrSessionNotFound
	}
	if s.closed.Load() {
		return ErrSessionClosed
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		s.closeWith(code, reason, initiatorStack)
	}()
	return nil
}
