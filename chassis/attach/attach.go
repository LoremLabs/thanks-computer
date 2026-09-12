// Package attach is the seam behind an attached transport: a live resource
// inside a workspace bound to one external connection for as long as a
// lease allows, with the stack having authorized the binding ONCE rather
// than running on every byte.
//
// Two Kinds exist. "pty" — `workspace://<name>/attach` binds a
// pseudo-terminal to the WebSocket session whose run made the op, and from
// then on the personality pumps frames straight to and from the process.
// "service" — `workspace://<name>/connect` binds a byte stream to one of
// the workspace's own loopback services (a VNC display, say), named from a
// chassis-owned table; the same registry, lease, metering and teardown
// funnel carry it, and the personality cannot tell the two apart.
//
// The processor creates bindings (it has the tenant, the workspace manager
// and the usage sink); the websocket personality pumps them (it has the
// socket). Neither imports the other's package for this — both import
// this one, the way processor.Unit.Workspaces is shared.
package attach

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// Kinds of attached resource.
const (
	KindPTY     = "pty"
	KindService = "service"
)

// Control message types a client may send on the text channel; the
// personality decodes the envelope and calls Conn.Control with the type
// and the raw JSON.
const (
	ControlResize = "resize"
	ControlSignal = "signal"
	ControlDetach = "detach" // handled by the personality: never reaches Conn
)

// Errors the Registry returns.
var (
	ErrAlreadyBound = errors.New("attach: session already has an attachment")
	ErrNotBound     = errors.New("attach: session has no attachment")
)

// Conn is one live resource bound to one external transport. Read is
// resource → client, Write is client → resource; Control carries the typed
// out-of-band messages (resize, signal); Wait reports how the resource
// ended. Close ends it, whatever it is doing, and is idempotent.
type Conn interface {
	io.ReadWriteCloser
	Control(ctx context.Context, typ string, raw []byte) error
	Wait() (exit int, err error)
}

// Params are the plain fields New needs to build an Attachment. Kept
// separate from Attachment so the atomics never travel by value (go vet).
type Params struct {
	ID        string // the lease id; the handle in logs and admin views
	Tenant    string
	AppStack  string // the workspace's owning stack (workspace.AppStack)
	Workspace string
	Kind      string // KindPTY | KindService
	Service   string // KindService: the service name the binding reached

	SessionID string // the WebSocket session holding the socket
	NodeID    string // the chassis node holding that socket
	RunID     string // the workspace run the resource lives in
	Computer  string // the provider's reference for the workspace

	StartedAt time.Time
	ExpiresAt time.Time
}

// Attachment is one granted binding: who holds it, what it is bound to, how
// long it may live, and the counters the lease's metering reads.
type Attachment struct {
	ID        string
	Tenant    string
	AppStack  string
	Workspace string
	Kind      string
	Service   string

	SessionID string
	NodeID    string
	RunID     string
	Computer  string

	StartedAt time.Time
	ExpiresAt time.Time

	// Conn is the live resource; set by the caller right after New (the
	// session is started on Context, which New must create first).
	Conn Conn

	// BytesIn counts client → resource bytes, BytesOut resource → client;
	// the pump adds, the heartbeat reads.
	BytesIn  atomic.Int64
	BytesOut atomic.Int64

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	closed    atomic.Bool
	reason    atomic.Pointer[string]
}

// New builds an attachment whose lifetime is its own context, derived from
// parent WITHOUT its cancellation: the op that grants a binding returns in
// milliseconds, and the binding must outlive that run. It ends on
// Close — detach, expiry, session teardown, shutdown.
func New(parent context.Context, p Params) *Attachment {
	att := &Attachment{
		ID: p.ID, Tenant: p.Tenant, AppStack: p.AppStack, Workspace: p.Workspace, Kind: p.Kind, Service: p.Service,
		SessionID: p.SessionID, NodeID: p.NodeID, RunID: p.RunID, Computer: p.Computer,
		StartedAt: p.StartedAt, ExpiresAt: p.ExpiresAt,
	}
	att.ctx, att.cancel = context.WithCancel(context.WithoutCancel(parent))
	return att
}

// Context ends when the attachment does. Start the session's process on it
// so that ending the attachment kills the process.
func (a *Attachment) Context() context.Context { return a.ctx }

// Close ends the attachment once: cancels the context (killing the
// process), closes the Conn, and records why. Safe from any goroutine.
func (a *Attachment) Close(reason string) {
	a.closeOnce.Do(func() {
		a.reason.Store(&reason)
		a.closed.Store(true)
		a.cancel()
		if a.Conn != nil {
			_ = a.Conn.Close()
		}
	})
}

// Closed reports whether Close has run.
func (a *Attachment) Closed() bool { return a.closed.Load() }

// Reason is why the attachment ended ("" while it lives).
func (a *Attachment) Reason() string {
	if p := a.reason.Load(); p != nil {
		return *p
	}
	return ""
}

// Expired reports whether the lease's max_duration has passed at now.
func (a *Attachment) Expired(now time.Time) bool {
	return !a.ExpiresAt.IsZero() && !now.Before(a.ExpiresAt)
}

// Registry maps a session to its one attachment. A session may hold at most
// one — a second attach on a bound session is refused, and the terminal's
// own multiplexing (tmux) is where several processes go.
type Registry struct {
	mu   sync.Mutex
	byID map[string]*Attachment // key: tenant \x00 session id
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry { return &Registry{byID: map[string]*Attachment{}} }

func key(tenant, sid string) string { return tenant + "\x00" + sid }

// Bind registers a for its session; ErrAlreadyBound if one is live there.
func (r *Registry) Bind(a *Attachment) error {
	if a == nil || a.Tenant == "" || a.SessionID == "" {
		return errors.New("attach: bind needs a tenant and a session id")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.byID[key(a.Tenant, a.SessionID)]; ok && !cur.Closed() {
		return ErrAlreadyBound
	}
	r.byID[key(a.Tenant, a.SessionID)] = a
	return nil
}

// Lookup returns the live attachment for a tenant's session. A session of
// another tenant is indistinguishable from an unbound one.
func (r *Registry) Lookup(tenant, sid string) (*Attachment, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byID[key(tenant, sid)]
	if !ok || a.Closed() {
		return nil, false
	}
	return a, true
}

// Unbind removes the session's attachment from the registry and returns it;
// the caller closes it. ok is false when there was none.
func (r *Registry) Unbind(tenant, sid string) (*Attachment, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.byID[key(tenant, sid)]
	if !ok {
		return nil, false
	}
	delete(r.byID, key(tenant, sid))
	return a, true
}

// List snapshots the live attachments, every tenant when tenant is "".
func (r *Registry) List(tenant string) []*Attachment {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Attachment, 0, len(r.byID))
	for _, a := range r.byID {
		if a.Closed() || (tenant != "" && a.Tenant != tenant) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// CloseAll ends every attachment (shutdown).
func (r *Registry) CloseAll(reason string) {
	for _, a := range r.List("") {
		a.Close(reason)
	}
}

// --- the PTY Conn ------------------------------------------------------------

// ptyConn adapts a workspace.ExecSession to Conn.
type ptyConn struct {
	sess workspace.ExecSession
}

// NewPTY binds a running TTY session as a Conn: Read drains the terminal,
// Write feeds it, resize and signal are the two controls.
func NewPTY(sess workspace.ExecSession) Conn { return &ptyConn{sess: sess} }

func (p *ptyConn) Read(b []byte) (int, error)  { return p.sess.Stdout().Read(b) }
func (p *ptyConn) Write(b []byte) (int, error) { return p.sess.Stdin().Write(b) }
func (p *ptyConn) Close() error                { return p.sess.Close() }

func (p *ptyConn) Wait() (int, error) {
	res, err := p.sess.Wait()
	return res.Exit, err
}

// Control decodes the D3 envelope for the two controls a terminal has.
func (p *ptyConn) Control(_ context.Context, typ string, raw []byte) error {
	switch typ {
	case ControlResize:
		var m struct {
			Cols uint16 `json:"cols"`
			Rows uint16 `json:"rows"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("resize: %w", err)
		}
		if m.Cols == 0 || m.Rows == 0 {
			return errors.New("resize: cols and rows must be positive")
		}
		return p.sess.Resize(m.Cols, m.Rows)
	case ControlSignal:
		var m struct {
			Signal string `json:"signal"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("signal: %w", err)
		}
		if !workspace.IsSignal(m.Signal) {
			return fmt.Errorf("signal: unknown %q (want one of %v)", m.Signal, workspace.ValidSignals)
		}
		return p.sess.Signal(m.Signal)
	}
	return fmt.Errorf("unknown control type %q", typ)
}

// --- the service Conn --------------------------------------------------------

// netConn adapts a net.Conn — a workspace-local service reached through
// the provider's Dialer — to Conn. A byte stream has no controls, so
// resize and signal are refused with an error the personality relays as
// one error frame; Wait reports exit 0 as soon as either side has closed,
// which is what the pump's end-of-output needs to send its exit frame.
type netConn struct {
	c    net.Conn
	done chan struct{}
	once sync.Once
}

// NewNet binds a dialed service connection as a Conn.
func NewNet(c net.Conn) Conn { return &netConn{c: c, done: make(chan struct{})} }

func (n *netConn) Read(b []byte) (int, error) {
	k, err := n.c.Read(b)
	if err != nil {
		n.end()
	}
	return k, err
}

func (n *netConn) Write(b []byte) (int, error) { return n.c.Write(b) }

func (n *netConn) Close() error {
	err := n.c.Close()
	n.end()
	return err
}

func (n *netConn) end() { n.once.Do(func() { close(n.done) }) }

func (n *netConn) Wait() (int, error) {
	<-n.done
	return 0, nil
}

func (n *netConn) Control(_ context.Context, typ string, _ []byte) error {
	return fmt.Errorf("control %q: a service connection carries bytes only (no resize or signal)", typ)
}
