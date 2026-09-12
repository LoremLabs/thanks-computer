package websocket

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/attach"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// stubConn is a controllable attach.Conn: output is fed on a channel, input
// and controls are recorded, and Wait blocks until the conn is closed or
// exit is delivered.
type stubConn struct {
	out    chan []byte
	exit   int
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	writes   []byte
	controls []string
}

func newStubConn() *stubConn {
	return &stubConn{out: make(chan []byte, 8), closed: make(chan struct{})}
}

func (c *stubConn) Read(p []byte) (int, error) {
	select {
	case b, ok := <-c.out:
		if !ok {
			return 0, io.EOF
		}
		return copy(p, b), nil
	case <-c.closed:
		return 0, io.EOF
	}
}

func (c *stubConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes = append(c.writes, p...)
	return len(p), nil
}

func (c *stubConn) Control(_ context.Context, typ string, _ []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.controls = append(c.controls, typ)
	return nil
}

func (c *stubConn) Wait() (int, error) { <-c.closed; return c.exit, nil }
func (c *stubConn) Close() error       { c.once.Do(func() { close(c.closed) }); return nil }

func (c *stubConn) wrote() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.writes)
}

// attachHarness binds a stub attachment the moment a message arrives, the
// way the attach op would inside a real stack run.
func attachHarness(t *testing.T, conn attach.Conn) (*harness, *attach.Registry) {
	t.Helper()
	reg := attach.NewRegistry()
	h := newHarness(t, nil, Accept{})
	h.ctrl.pu.Attachments = reg
	h.setReply(func(env *event.Envelope) event.Payload {
		sid := gjson.Get(env.Payload.Raw, "_txc.websocket.session.id").String()
		if _, bound := reg.Lookup("acme", sid); !bound && sid != "" {
			att := attach.New(context.Background(), attach.Params{
				ID: "att_test", Tenant: "acme", AppStack: "counter", Workspace: "tools",
				Kind: attach.KindPTY, SessionID: sid, NodeID: "node-a",
				StartedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
			})
			att.Conn = conn
			_ = reg.Bind(att)
		}
		return event.Payload{Raw: `{}`, Type: event.JSON}
	})
	return h, reg
}

// TestAttachPumpsBothWays: once bound, terminal output reaches the client as
// binary, client binary reaches the process's stdin, a resize control is
// delivered, and the process exit closes the socket with an exit frame.
func TestAttachPumpsBothWays(t *testing.T) {
	conn := newStubConn()
	h, reg := attachHarness(t, conn)
	c := h.dial(t)
	defer c.CloseNow()

	// First message attaches (the reply binds the stub).
	writeText(t, c, `{"type":"attach"}`)

	// Wait for the binding to be live, then feed output.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := reg.Lookup("acme", firstSID(h)); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attachment never bound")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := readText(t, c); got != `{"type":"attached"}` {
		t.Fatalf("first frame = %q, want the attached ack", got)
	}
	conn.out <- []byte("hello-term")
	if got := readBinary(t, c); got != "hello-term" {
		t.Fatalf("pump output = %q, want hello-term", got)
	}

	// Client → process stdin.
	writeBinary(t, c, "keystroke")
	// Control: resize.
	writeText(t, c, `{"type":"resize","cols":100,"rows":30}`)
	waitFor(t, "stdin+resize delivered", func() bool { return conn.wrote() == "keystroke" && len(conn.controls) == 1 })
	if conn.controls[0] != "resize" {
		t.Errorf("control = %v, want resize", conn.controls)
	}

	// Process exits → exit frame, socket closes 1000.
	conn.exit = 0
	conn.Close()
	if got := readText(t, c); got != `{"type":"exit","code":0}` {
		t.Fatalf("exit frame = %q", got)
	}
	expectClose(t, c, websocket.StatusNormalClosure)
}

// TestSendToAttachedRefused: txco://websocket/send to an attached session is
// refused with ErrSessionAttached (D5).
func TestSendToAttachedRefused(t *testing.T) {
	conn := newStubConn()
	h, reg := attachHarness(t, conn)
	c := h.dial(t)
	defer c.CloseNow()
	writeText(t, c, `{"type":"attach"}`)
	waitFor(t, "attachment bound", func() bool { _, ok := reg.Lookup("acme", firstSID(h)); return ok })

	err := h.ctrl.Send(context.Background(), "acme", firstSID(h), MessageText, []byte("x"))
	if !errors.Is(err, ErrSessionAttached) {
		t.Fatalf("Send to attached = %v, want ErrSessionAttached", err)
	}
	conn.Close()
}

// TestDetachReturnsToMessageMode: a {"type":"detach"} ends the binding and
// the socket keeps serving ordinary message runs.
func TestDetachReturnsToMessageMode(t *testing.T) {
	conn := newStubConn()
	h, reg := attachHarness(t, conn)
	c := h.dial(t)
	defer c.CloseNow()
	writeText(t, c, `{"type":"attach"}`)
	waitFor(t, "attachment bound", func() bool { _, ok := reg.Lookup("acme", firstSID(h)); return ok })

	writeText(t, c, `{"type":"detach"}`)
	waitFor(t, "attachment gone", func() bool { _, ok := reg.Lookup("acme", firstSID(h)); return !ok })

	// The socket still works: a further message runs the stack again.
	before := len(h.envelopes())
	writeText(t, c, `{"type":"ping"}`)
	waitFor(t, "message ran after detach", func() bool { return len(h.envelopes()) > before })
}

func firstSID(h *harness) string {
	for _, env := range h.envelopes() {
		if sid := gjson.Get(env.Payload.Raw, "_txc.websocket.session.id").String(); sid != "" {
			return sid
		}
	}
	return ""
}

func readBinary(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("type = %v, want binary", typ)
	}
	return string(data)
}

func writeBinary(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestConnectPumpsServiceConn: a service binding (attach.NewNet over a
// pipe) rides the same frame path as a PTY — bytes both ways, a resize
// control answered with one error frame and the session kept, and the far
// side closing ends the socket with an exit frame — with no change to the
// personality.
func TestConnectPumpsServiceConn(t *testing.T) {
	near, far := net.Pipe()
	defer far.Close()
	h, reg := attachHarness(t, attach.NewNet(near))
	c := h.dial(t)
	defer c.CloseNow()

	writeText(t, c, `{"type":"connect","service":"browser"}`)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := reg.Lookup("acme", firstSID(h)); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("attachment never bound")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := readText(t, c); got != `{"type":"attached"}` {
		t.Fatalf("first frame = %q, want the attached ack", got)
	}

	// Service → client.
	go func() { _, _ = far.Write([]byte("RFB 003.008\n")) }()
	if got := readBinary(t, c); got != "RFB 003.008\n" {
		t.Fatalf("pump output = %q", got)
	}
	// Client → service.
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 16)
		n, _ := far.Read(b)
		got <- string(b[:n])
	}()
	writeBinary(t, c, "RFB 003.008\n")
	select {
	case s := <-got:
		if s != "RFB 003.008\n" {
			t.Fatalf("service read = %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client bytes never reached the service")
	}
	// A control has nowhere to go on a byte stream: one error frame, session stays.
	writeText(t, c, `{"type":"resize","cols":100,"rows":30}`)
	if got := readText(t, c); !strings.Contains(got, `"type":"error"`) || !strings.Contains(got, "bytes only") {
		t.Fatalf("resize on a service = %q", got)
	}
	// The service closing ends the binding: exit frame, close 1000.
	_ = far.Close()
	if got := readText(t, c); got != `{"type":"exit","code":0}` {
		t.Fatalf("exit frame = %q", got)
	}
	expectClose(t, c, websocket.StatusNormalClosure)
}
