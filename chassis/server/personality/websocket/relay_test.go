package websocket

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
)

// fabric stands in for the message bus: it routes a relay request to the
// LocalHandler registered under the target node token, or reports
// no-responders (ErrSessionNotFound) when nothing is listening there.
type fabric struct {
	mu    sync.Mutex
	nodes map[string]LocalHandler
}

func newFabric() *fabric { return &fabric{nodes: map[string]LocalHandler{}} }

func (f *fabric) add(node string, h LocalHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[node] = h
}

func (f *fabric) remove(node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nodes, node)
}

func (f *fabric) handler(node string) (LocalHandler, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.nodes[node]
	return h, ok
}

type relayCall struct {
	op, node, tenant, sid string
	data                  []byte
	code                  int
}

// fakeRelay records every request and delivers it through the fabric —
// the shape of chassis/room's fakeRelay, plus the answer a real request
// carries back.
type fakeRelay struct {
	f    *fabric
	node string

	mu       sync.Mutex
	calls    []relayCall
	shutdown bool
}

func (r *fakeRelay) Node() string { return r.node }

func (r *fakeRelay) Send(ctx context.Context, node, tenant, sid string, typ MessageType, data []byte) error {
	r.mu.Lock()
	r.calls = append(r.calls, relayCall{op: "send", node: node, tenant: tenant, sid: sid, data: data})
	r.mu.Unlock()
	h, ok := r.f.handler(node)
	if !ok {
		return ErrSessionNotFound // no responders on that inbox
	}
	return h.Send(ctx, tenant, sid, typ, data)
}

func (r *fakeRelay) Close(ctx context.Context, node, tenant, sid string, code int, reason string) error {
	r.mu.Lock()
	r.calls = append(r.calls, relayCall{op: "close", node: node, tenant: tenant, sid: sid, code: code})
	r.mu.Unlock()
	h, ok := r.f.handler(node)
	if !ok {
		return ErrSessionNotFound
	}
	return h.Close(ctx, tenant, sid, code, reason)
}

func (r *fakeRelay) Shutdown(context.Context) error {
	r.f.remove(r.node)
	r.mu.Lock()
	r.shutdown = true
	r.mu.Unlock()
	return nil
}

func (r *fakeRelay) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// memDirectory is an in-memory Directory shared by the nodes of a test.
type memDirectory struct {
	mu         sync.Mutex
	entries    map[string]directoryEntry // tenant + "/" + sid
	ops        []string
	failLookup error
}

func newMemDirectory() *memDirectory { return &memDirectory{entries: map[string]directoryEntry{}} }

func (d *memDirectory) key(tenant, sid string) string { return tenant + "/" + sid }

func (d *memDirectory) Register(_ context.Context, tenant, sid, node, stack string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[d.key(tenant, sid)] = directoryEntry{Node: node, Stack: stack}
	d.ops = append(d.ops, "register")
	return nil
}

func (d *memDirectory) Refresh(_ context.Context, tenant, sid, node, stack string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.entries[d.key(tenant, sid)] = directoryEntry{Node: node, Stack: stack}
	d.ops = append(d.ops, "refresh")
	return nil
}

func (d *memDirectory) Unregister(_ context.Context, tenant, sid string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, d.key(tenant, sid))
	d.ops = append(d.ops, "unregister")
	return nil
}

func (d *memDirectory) Lookup(_ context.Context, tenant, sid string) (string, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failLookup != nil {
		return "", false, d.failLookup
	}
	e, ok := d.entries[d.key(tenant, sid)]
	if !ok {
		return "", false, nil
	}
	return e.Node, true, nil
}

func (d *memDirectory) get(tenant, sid string) (directoryEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[d.key(tenant, sid)]
	return e, ok
}

func (d *memDirectory) opCount(op string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, o := range d.ops {
		if o == op {
			n++
		}
	}
	return n
}

// node is one chassis: a harness whose controller is wired to the shared
// fabric + directory under a node token.
type node struct {
	*harness
	relay *fakeRelay
}

func newNode(t *testing.T, name string, f *fabric, d Directory, acc Accept) *node {
	t.Helper()
	r := &fakeRelay{f: f, node: name}
	h := newHarnessWith(t, nil, acc, func(c *Controller) {
		c.SetRelay(r, d)
		f.add(name, c.Local())
	})
	return &node{harness: h, relay: r}
}

func sessionID(t *testing.T, h *harness, c *websocket.Conn) string {
	t.Helper()
	writeText(t, c, "hello")
	envs := h.waitEnvelopes(t, 1)
	return gjson.Get(envs[len(envs)-1].Payload.Raw, "_txc.websocket.session.id").String()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRemoteSendAndCloseReachOwningNode(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	a := newNode(t, "nodeA", f, d, Accept{})
	b := newNode(t, "nodeB", f, d, Accept{})

	c := b.dial(t) // the socket lives on B
	defer c.CloseNow()
	sid := sessionID(t, b.harness, c)

	e, ok := d.get("acme", sid)
	if !ok || e.Node != "nodeB" || e.Stack != "counter" {
		t.Fatalf("directory entry = %+v ok=%v, want nodeB/counter", e, ok)
	}

	// A does not hold the socket; the send resolves through the directory.
	if err := a.ctrl.Send(context.Background(), "acme", sid, MessageText, []byte("from A")); err != nil {
		t.Fatalf("remote send: %v", err)
	}
	if got := readText(t, c); got != "from A" {
		t.Fatalf("client got %q", got)
	}
	if a.relay.count() != 1 || a.relay.calls[0].node != "nodeB" {
		t.Fatalf("relay calls = %+v", a.relay.calls)
	}
	// Binary survives the hop too.
	if err := a.ctrl.Send(context.Background(), "acme", sid, MessageBinary, []byte{7, 8, 9}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || len(data) != 3 {
		t.Fatalf("binary: %v %v %v", typ, data, err)
	}

	// A closes B's session.
	if err := a.ctrl.CloseSession(context.Background(), "acme", sid, 4001, "from A"); err != nil {
		t.Fatalf("remote close: %v", err)
	}
	ce := expectClose(t, c, websocket.StatusCode(4001))
	if ce.Reason != "from A" {
		t.Errorf("reason = %q", ce.Reason)
	}
	waitFor(t, "directory entry removed on close", func() bool { _, ok := d.get("acme", sid); return !ok })
	// And now it is a plain miss from both nodes, with no relay traffic.
	before := a.relay.count()
	if err := a.ctrl.Send(context.Background(), "acme", sid, MessageText, []byte("late")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("send after close = %v", err)
	}
	if a.relay.count() != before {
		t.Fatal("a miss in the directory must not touch the relay")
	}
}

func TestRemoteUnknownSessionIsImmediateMiss(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	a := newNode(t, "nodeA", f, d, Accept{})
	start := time.Now()
	if err := a.ctrl.Send(context.Background(), "acme", "ws_nope", MessageText, []byte("x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("an unknown session must miss without waiting on the relay")
	}
	if a.relay.count() != 0 {
		t.Fatal("relay called for an unknown session")
	}
}

func TestRemoteStaleEntryIsForgotten(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	a := newNode(t, "nodeA", f, d, Accept{})
	// An entry for a node that is gone (a destroyed Machine): the relay
	// answers no-responders, the entry is dropped.
	_ = d.Register(context.Background(), "acme", "ws_gone", "nodeC", "counter")
	if err := a.ctrl.Send(context.Background(), "acme", "ws_gone", MessageText, []byte("x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
	if a.relay.count() != 1 {
		t.Fatalf("relay calls = %d, want 1", a.relay.count())
	}
	if _, ok := d.get("acme", "ws_gone"); ok {
		t.Fatal("stale entry not forgotten after no-responders")
	}
	// An entry naming THIS node with no local session: stale self, no relay.
	_ = d.Register(context.Background(), "acme", "ws_self", "nodeA", "counter")
	if err := a.ctrl.CloseSession(context.Background(), "acme", "ws_self", 1000, ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
	if a.relay.count() != 1 {
		t.Fatal("relay called for a stale self entry")
	}
	if _, ok := d.get("acme", "ws_self"); ok {
		t.Fatal("stale self entry not forgotten")
	}
}

func TestRemoteDirectoryErrorIsRelayUnavailable(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	a := newNode(t, "nodeA", f, d, Accept{})
	d.mu.Lock()
	d.failLookup = errors.New("redis down")
	d.mu.Unlock()
	if err := a.ctrl.Send(context.Background(), "acme", "ws_x", MessageText, []byte("x")); !errors.Is(err, ErrRelayUnavailable) {
		t.Fatalf("err = %v, want ErrRelayUnavailable", err)
	}
	if a.relay.count() != 0 {
		t.Fatal("relay called while the directory is down")
	}
}

func TestRemoteCrossTenantIsMiss(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	a := newNode(t, "nodeA", f, d, Accept{})
	b := newNode(t, "nodeB", f, d, Accept{})
	c := b.dial(t)
	defer c.CloseNow()
	sid := sessionID(t, b.harness, c)
	if err := a.ctrl.Send(context.Background(), "mallory", sid, MessageText, []byte("x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant err = %v", err)
	}
	if a.relay.count() != 0 {
		t.Fatal("a foreign tenant's lookup must miss in the directory, never reach the relay")
	}
}

func TestRemoteAnswersCarryOwnerErrors(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	a := newNode(t, "nodeA", f, d, Accept{})
	b := newNode(t, "nodeB", f, d, Accept{})
	c := b.dial(t)
	defer c.CloseNow()
	sid := sessionID(t, b.harness, c)
	if err := a.ctrl.Send(context.Background(), "acme", sid, MessageText, make([]byte, 300000)); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversize via relay = %v", err)
	}
	if _, ok := d.get("acme", sid); !ok {
		t.Fatal("an owner-side error must not drop a live entry")
	}
}

func TestLeaseRefresh(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	b := newNode(t, "nodeB", f, d, Accept{})
	c := b.dial(t)
	defer c.CloseNow()
	_ = sessionID(t, b.harness, c)
	if d.opCount("register") != 1 || d.opCount("refresh") != 0 {
		t.Fatalf("ops = %v", d.ops)
	}
	b.ctrl.refreshLeases(time.Now()) // young lease: nothing to do
	if d.opCount("refresh") != 0 {
		t.Fatal("refreshed a young lease")
	}
	b.ctrl.refreshLeases(time.Now().Add(b.ctrl.LeaseTTL())) // past half-life
	if d.opCount("refresh") != 1 {
		t.Fatalf("refresh count = %d, want 1", d.opCount("refresh"))
	}
	b.ctrl.refreshLeases(time.Now().Add(b.ctrl.LeaseTTL())) // just refreshed at "now"
	if d.opCount("refresh") != 1 {
		t.Fatal("refreshed twice for one half-life")
	}
}

func TestStopUnregistersAndShutsRelayDown(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	b := newNode(t, "nodeB", f, d, Accept{})
	c := b.dial(t)
	defer c.CloseNow()
	sid := sessionID(t, b.harness, c)
	// Stop waits for the close handshake, which needs the client reading.
	stopped := make(chan struct{})
	go func() {
		b.ctrl.Stop()
		close(stopped)
	}()
	expectClose(t, c, websocket.StatusGoingAway)
	<-stopped
	if _, ok := d.get("acme", sid); ok {
		t.Fatal("entry survived Stop")
	}
	b.relay.mu.Lock()
	down := b.relay.shutdown
	b.relay.mu.Unlock()
	if !down {
		t.Fatal("relay not shut down")
	}
	if _, ok := f.handler("nodeB"); ok {
		t.Fatal("node still reachable on the fabric after Stop")
	}
}

func TestLocalHandlerNeverGoesRemote(t *testing.T) {
	f, d := newFabric(), newMemDirectory()
	a := newNode(t, "nodeA", f, d, Accept{})
	_ = d.Register(context.Background(), "acme", "ws_elsewhere", "nodeB", "counter")
	// The inbound surface resolves locally only: an entry for another node
	// must not make it bounce the request back out.
	if err := a.ctrl.Local().Send(context.Background(), "acme", "ws_elsewhere", MessageText, []byte("x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
	if a.relay.count() != 0 {
		t.Fatal("LocalHandler reached the relay")
	}
}

func TestWithoutRelayBehaviorIsUnchanged(t *testing.T) {
	h := newHarness(t, nil, Accept{})
	if h.ctrl.crossNode() {
		t.Fatal("crossNode without SetRelay")
	}
	if err := h.ctrl.Send(context.Background(), "acme", "ws_x", MessageText, []byte("x")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("err = %v", err)
	}
	h.ctrl.refreshLeases(time.Now()) // no-op, no panic
	if !strings.HasPrefix(h.ctrl.NewSessionID(), "ws_") {
		t.Fatal("ids unchanged")
	}
}

// keep the event import used when the harness helpers evolve.
var _ = event.JSON
