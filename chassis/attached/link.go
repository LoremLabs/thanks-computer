package attached

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Handler is what the machine does with what comes down the link. The link
// itself runs nothing: it carries a request to the handler and the answer
// back.
type Handler interface {
	// Request does the work in h.Payload and returns the reply. ctx ends at
	// the request's deadline, or sooner when the chassis cancels it.
	Request(ctx context.Context, h Header) Reply
	// Stop asks a detached job to end.
	Stop(ctx context.Context, job string) error
}

// Link is the machine's side of one credential's connection.
type Link struct {
	Credential string
	Key        ed25519.PrivateKey
	// Host is the name this machine dialed. It is signed into the
	// challenge, so an answer cannot be relayed to another host.
	Host string
	// What the machine says of itself. Shown to the owner on the chassis's
	// side; decides nothing.
	Machine, Root, OS, Version string

	Handler Handler
	// OnWelcome, when set, is called once the chassis has accepted the link.
	OnWelcome func(Welcome)

	mu      sync.Mutex
	sess    *yamux.Session
	running map[string]context.CancelFunc
	seen    map[string]time.Time
}

// ErrLinkDown: there is no connection to send on right now.
var ErrLinkDown = errors.New("attached: the link is down")

// Refused is a link the chassis turned away or closed, with its reason. A
// machine told ReasonReplaced or ReasonRevoked stops serving the credential:
// it does not dial again.
type Refused struct{ Reason string }

func (r *Refused) Error() string { return "attached: the chassis closed the link: " + r.Reason }

// Final reports whether err says this credential should not be tried again.
func Final(err error) bool {
	var r *Refused
	return errors.As(err, &r) && (r.Reason == ReasonReplaced || r.Reason == ReasonRevoked)
}

// Run serves one connection, from the handshake until it ends. It returns a
// *Refused when the chassis said why, and the connection's own error
// otherwise. Requests still running when it ends are cancelled; detached
// jobs are not this link's to end, and run on.
func (l *Link) Run(ctx context.Context, rw io.ReadWriteCloser) error {
	sess, err := yamux.Client(rw, yamuxConfig())
	if err != nil {
		_ = rw.Close()
		return err
	}
	defer sess.Close()
	stopCtx := context.AfterFunc(ctx, func() { _ = sess.Close() })
	defer stopCtx()

	welcome, err := l.hello(sess)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends every request this connection was running

	l.mu.Lock()
	l.sess = sess
	if l.running == nil {
		l.running = map[string]context.CancelFunc{}
		l.seen = map[string]time.Time{}
	}
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.sess == sess {
			l.sess = nil
		}
		l.mu.Unlock()
	}()
	if l.OnWelcome != nil {
		l.OnWelcome(welcome)
	}

	var bye struct {
		sync.Mutex
		reason string
	}
	var wg sync.WaitGroup
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			// The connection is over. End what it was running, and wait for
			// every stream's handler: one of them may hold the reason.
			cancel()
			wg.Wait()
			bye.Lock()
			reason := bye.reason
			bye.Unlock()
			if reason != "" {
				return &Refused{Reason: reason}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reason := l.serve(ctx, st); reason != "" {
				bye.Lock()
				bye.reason = reason
				bye.Unlock()
			}
		}()
	}
}

// hello proves this machine holds the credential's key on this connection.
func (l *Link) hello(sess *yamux.Session) (Welcome, error) {
	st, err := sess.OpenStream()
	if err != nil {
		return Welcome{}, err
	}
	defer st.Close()
	_ = st.SetDeadline(time.Now().Add(handshakeTimeout))
	in := newMsgReader(st)
	if err := writeMsg(st, Header{Kind: KindHello, Credential: l.Credential,
		Machine: l.Machine, Root: l.Root, OS: l.OS, Version: l.Version}); err != nil {
		return Welcome{}, err
	}
	var ch Challenge
	if err := in.read(&ch); err != nil {
		return Welcome{}, fmt.Errorf("attached: no challenge: %w", err)
	}
	if ch.Refused != "" {
		return Welcome{}, &Refused{Reason: ch.Refused}
	}
	sig := ed25519.Sign(l.Key, ChallengeBytes(l.Host, l.Credential, ch.Conn, ch.Nonce))
	if err := writeMsg(st, Answer{Signature: sig}); err != nil {
		return Welcome{}, err
	}
	var w Welcome
	if err := in.read(&w); err != nil {
		return Welcome{}, fmt.Errorf("attached: no welcome: %w", err)
	}
	if !w.OK {
		return Welcome{}, &Refused{Reason: w.Reason}
	}
	return w, nil
}

// serve handles one stream the chassis opened. It returns the reason when
// the stream was the chassis saying why it is closing the link.
func (l *Link) serve(ctx context.Context, st *yamux.Stream) (bye string) {
	defer st.Close()
	var h Header
	if err := newMsgReader(st).read(&h); err != nil {
		return ""
	}
	switch h.Kind {
	case KindBye:
		return h.Reason
	case KindRequest:
		_ = writeMsg(st, l.request(ctx, h))
	case KindCancel:
		l.mu.Lock()
		cancel := l.running[h.ID]
		l.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		_ = writeMsg(st, Reply{OK: true})
	case KindStop:
		if err := l.Handler.Stop(ctx, h.Job); err != nil {
			_ = writeMsg(st, Reply{Error: &WireError{Code: "provider", Message: err.Error()}})
			return ""
		}
		_ = writeMsg(st, Reply{OK: true})
	default:
		_ = writeMsg(st, Reply{Error: &WireError{Code: "bad_request", Message: "unknown stream kind " + h.Kind}})
	}
	return ""
}

// request runs one request once. An id seen in the last hour is refused
// without running: the chassis never resends, so a repeat is a fault
// somewhere, and running a command twice is the worse answer to it.
func (l *Link) request(ctx context.Context, h Header) Reply {
	if h.ID == "" {
		return Reply{Error: &WireError{Code: "bad_request", Message: "a request needs an id"}}
	}
	now := time.Now()
	l.mu.Lock()
	if len(l.seen) > 4096 {
		for k, t := range l.seen {
			if now.Sub(t) > time.Hour {
				delete(l.seen, k)
			}
		}
	}
	if t, ok := l.seen[h.ID]; ok && now.Sub(t) <= time.Hour {
		l.mu.Unlock()
		return Reply{Error: &WireError{Code: "unknown", Message: "a request with this id was already received; it is not run again"}}
	}
	l.seen[h.ID] = now
	rctx, cancel := context.WithCancel(ctx)
	if !h.Deadline.IsZero() {
		rctx, cancel = context.WithDeadline(ctx, h.Deadline)
	}
	l.running[h.ID] = cancel
	l.mu.Unlock()
	defer func() {
		cancel()
		l.mu.Lock()
		delete(l.running, h.ID)
		l.mu.Unlock()
	}()
	return l.Handler.Request(rctx, h)
}

// Complete sends a detached job's end up the link and returns nil once the
// chassis has acknowledged it — which it does only after recording the
// result durably. Until then the caller keeps its copy and sends again,
// on this connection or the next.
func (l *Link) Complete(ctx context.Context, h Header) error {
	l.mu.Lock()
	sess := l.sess
	l.mu.Unlock()
	if sess == nil {
		return ErrLinkDown
	}
	st, err := sess.OpenStream()
	if err != nil {
		return ErrLinkDown
	}
	defer st.Close()
	stop := context.AfterFunc(ctx, func() { _ = st.SetDeadline(time.Now()) })
	defer stop()
	h.Kind = KindComplete
	if err := writeMsg(st, h); err != nil {
		return err
	}
	var rep Reply
	if err := newMsgReader(st).read(&rep); err != nil {
		return err
	}
	if !rep.OK {
		if rep.Error != nil {
			return rep.Error.err()
		}
		return errors.New("attached: the completion was not acknowledged")
	}
	return nil
}
