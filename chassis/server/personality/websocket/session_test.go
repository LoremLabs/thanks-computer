package websocket

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// harness: a real listener whose handler records an accept and upgrades,
// a fake processor on the bus, and a coder client. No stack, no txcl —
// the stack's side is the accept the handler pre-records.
type harness struct {
	t    *testing.T
	ctrl *Controller
	bus  chan *event.Envelope
	srv  *httptest.Server
	acc  Accept

	mu    sync.Mutex
	seen  []*event.Envelope
	reply func(env *event.Envelope) event.Payload
}

func newHarness(t *testing.T, tune func(*config.Config), acc Accept) *harness {
	t.Helper()
	return newHarnessWith(t, tune, acc, nil)
}

// newHarnessWith also runs setup on the controller before Start — the hook
// the cross-node tests use to wire a relay + directory.
func newHarnessWith(t *testing.T, tune func(*config.Config), acc Accept, setup func(*Controller)) *harness {
	t.Helper()
	conf := config.Config{
		Personalities:              "web,websocket",
		WebsocketMaxConns:          0,
		WebsocketMaxConnsPerTenant: 0,
		WebsocketMaxMessageBytes:   262144,
		WebsocketInboundQueue:      16,
		WebsocketRunTimeout:        "2s",
		WebsocketIdleTimeout:       "10s",
		WebsocketMaxIdleTimeout:    "1h",
		WebsocketPingInterval:      "200ms",
		WebsocketWriteTimeout:      "500ms",
		WebsocketDrainTimeout:      "2s",
	}
	if tune != nil {
		tune(&conf)
	}
	bus := make(chan *event.Envelope, 8)
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Bus: bus}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu)
	if setup != nil {
		setup(ctrl)
	}
	ctrl.Start()
	if acc.Tenant == "" {
		acc.Tenant = "acme"
	}
	if acc.Stack == "" {
		acc.Stack = "counter"
	}
	h := &harness{t: t, ctrl: ctrl, bus: bus, acc: acc}
	h.reply = func(*event.Envelope) event.Payload { return event.Payload{Raw: `{}`, Type: event.JSON} }
	go func() {
		for env := range bus {
			if env == nil {
				return
			}
			h.mu.Lock()
			h.seen = append(h.seen, env)
			r := h.reply
			h.mu.Unlock()
			env.ResCh <- r(env)
		}
	}()
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := ctrl.NewSessionID()
		if err := ctrl.RecordAccept(sid, h.accept()); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		a, ok := ctrl.Claim(sid)
		if !ok {
			http.Error(w, "claim failed", 500)
			return
		}
		ctrl.Upgrade(w, r, sid, a)
	}))
	t.Cleanup(func() {
		ctrl.Stop()
		h.srv.Close()
		cancel()
	})
	return h
}

func (h *harness) dial(t *testing.T) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http")+"/ws", nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	return c
}

func (h *harness) accept() Accept {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.acc
}

func (h *harness) setAccept(a Accept) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.Tenant == "" {
		a.Tenant = "acme"
	}
	if a.Stack == "" {
		a.Stack = "counter"
	}
	h.acc = a
}

func (h *harness) setReply(f func(env *event.Envelope) event.Payload) {
	h.mu.Lock()
	h.reply = f
	h.mu.Unlock()
}

func (h *harness) envelopes() []*event.Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*event.Envelope, len(h.seen))
	copy(out, h.seen)
	return out
}

func (h *harness) waitEnvelopes(t *testing.T, n int) []*event.Envelope {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.envelopes(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("saw %d envelopes, want %d", len(h.envelopes()), n)
	return nil
}

func readText(t *testing.T, c *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("type = %v, want text", typ)
	}
	return string(data)
}

func expectClose(t *testing.T, c *websocket.Conn, code websocket.StatusCode) websocket.CloseError {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	if err == nil {
		t.Fatal("read succeeded, want close")
	}
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		t.Fatalf("read err = %v, want CloseError", err)
	}
	if ce.Code != code {
		t.Fatalf("close code = %d (%q), want %d", ce.Code, ce.Reason, code)
	}
	return ce
}

func writeText(t *testing.T, c *websocket.Conn, s string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(s)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// --- the spike: a message makes one run; the run replies through the registry

func TestMessageRunEnvelopeAndReply(t *testing.T) {
	h := newHarness(t, nil, Accept{State: `{"email":"a@b.c"}`})
	h.setReply(func(env *event.Envelope) event.Payload {
		raw := env.Payload.Raw
		sid := gjson.Get(raw, "_txc.websocket.session.id").String()
		text := gjson.Get(raw, "_txc.websocket.msg.text").String()
		if err := h.ctrl.Send(context.Background(), "acme", sid, MessageText, []byte("echo:"+text)); err != nil {
			t.Errorf("Send: %v", err)
		}
		return event.Payload{Raw: `{}`, Type: event.JSON}
	})
	c := h.dial(t)
	defer c.CloseNow()

	writeText(t, c, "hi")
	if got := readText(t, c); got != "echo:hi" {
		t.Fatalf("reply = %q", got)
	}
	writeText(t, c, "again")
	if got := readText(t, c); got != "echo:again" {
		t.Fatalf("reply = %q", got)
	}

	envs := h.waitEnvelopes(t, 2)
	raw := envs[0].Payload.Raw
	if envs[0].Src != Source {
		t.Errorf("envelope.Src = %q", envs[0].Src)
	}
	for path, want := range map[string]string{
		"_txc.src":                           "websocket",
		"_txc.route.to":                      "counter/_websocket/0",
		"_txc.route.stack":                   "counter/_websocket",
		"_txc.route.tenant":                  "acme",
		"_txc.websocket.phase":               "message",
		"_txc.websocket.msg.text":            "hi",
		"_txc.websocket.session.state.email": "a@b.c",
		"_txc.websocket.req.path":            "/ws",
	} {
		if got := gjson.Get(raw, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if !strings.HasPrefix(gjson.Get(raw, "_txc.websocket.session.id").String(), "ws_") {
		t.Errorf("session id = %q", gjson.Get(raw, "_txc.websocket.session.id").String())
	}
	if gjson.Get(raw, "_txc.client.ip").String() == "" {
		t.Error("client ip not stamped")
	}
	if s1, s2 := gjson.Get(envs[0].Payload.Raw, "_txc.websocket.session.seq").Int(), gjson.Get(envs[1].Payload.Raw, "_txc.websocket.session.seq").Int(); s1 != 1 || s2 != 2 {
		t.Errorf("seq = %d,%d want 1,2", s1, s2)
	}
	if h.ctrl.Count() != 1 {
		t.Errorf("open sessions = %d", h.ctrl.Count())
	}
}

func TestSendToOtherTenantIsNotFound(t *testing.T) {
	h := newHarness(t, nil, Accept{})
	c := h.dial(t)
	defer c.CloseNow()
	writeText(t, c, "x")
	sid := gjson.Get(h.waitEnvelopes(t, 1)[0].Payload.Raw, "_txc.websocket.session.id").String()
	if err := h.ctrl.Send(context.Background(), "mallory", sid, MessageText, []byte("hi")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant send err = %v, want ErrSessionNotFound", err)
	}
	if err := h.ctrl.Send(context.Background(), "acme", "ws_nope", MessageText, []byte("hi")); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unknown id err = %v", err)
	}
	if err := h.ctrl.Send(context.Background(), "acme", sid, MessageText, make([]byte, 300000)); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversize send err = %v", err)
	}
}

func TestInboundQueueOverflowCloses1013(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, func(c *config.Config) { c.WebsocketInboundQueue = 2 }, Accept{})
	h.setReply(func(env *event.Envelope) event.Payload {
		<-release // the first run blocks; the queue fills behind it
		return event.Payload{Raw: `{}`, Type: event.JSON}
	})
	c := h.dial(t)
	defer c.CloseNow()
	for i := 0; i < 6; i++ {
		writeText(t, c, "m")
	}
	ce := expectClose(t, c, websocket.StatusTryAgainLater)
	if !strings.Contains(ce.Reason, "queue") {
		t.Errorf("reason = %q", ce.Reason)
	}
	close(release)
}

func TestOversizeMessageCloses1009(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.WebsocketMaxMessageBytes = 64 }, Accept{})
	c := h.dial(t)
	defer c.CloseNow()
	writeText(t, c, strings.Repeat("x", 200))
	expectClose(t, c, websocket.StatusMessageTooBig)
}

func TestIdleTimeoutCloses1000(t *testing.T) {
	h := newHarness(t, func(c *config.Config) {
		c.WebsocketIdleTimeout = "300ms"
		c.WebsocketPingInterval = "50ms"
	}, Accept{})
	c := h.dial(t)
	defer c.CloseNow()
	ce := expectClose(t, c, websocket.StatusNormalClosure)
	if ce.Reason != "idle timeout" {
		t.Errorf("reason = %q", ce.Reason)
	}
}

func TestCloseEventOptIn(t *testing.T) {
	h := newHarness(t, nil, Accept{Events: map[string]bool{EventClose: true}})
	c := h.dial(t)
	writeText(t, c, "one")
	h.waitEnvelopes(t, 1)
	if err := c.Close(websocket.StatusNormalClosure, "bye"); err != nil {
		t.Fatalf("client close: %v", err)
	}
	envs := h.waitEnvelopes(t, 2)
	raw := envs[1].Payload.Raw
	if got := gjson.Get(raw, "_txc.websocket.phase").String(); got != "close" {
		t.Fatalf("phase = %q", got)
	}
	if got := gjson.Get(raw, "_txc.websocket.close.code").Int(); got != 1000 {
		t.Errorf("close.code = %d", got)
	}
	if got := gjson.Get(raw, "_txc.websocket.close.reason").String(); got != "bye" {
		t.Errorf("close.reason = %q", got)
	}
	if got := gjson.Get(raw, "_txc.websocket.close.initiated_by").String(); got != "client" {
		t.Errorf("initiated_by = %q", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.ctrl.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.ctrl.Count() != 0 {
		t.Errorf("session still registered after close")
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(h.envelopes()); n != 2 {
		t.Errorf("close run fired %d times, want once", n-1)
	}
}

func TestNoCloseEventWithoutOptIn(t *testing.T) {
	h := newHarness(t, nil, Accept{})
	c := h.dial(t)
	writeText(t, c, "one")
	h.waitEnvelopes(t, 1)
	_ = c.Close(websocket.StatusNormalClosure, "bye")
	time.Sleep(100 * time.Millisecond)
	if n := len(h.envelopes()); n != 1 {
		t.Fatalf("envelopes = %d, want 1 (no close run)", n)
	}
}

func TestStopCloses1001(t *testing.T) {
	h := newHarness(t, nil, Accept{})
	c := h.dial(t)
	defer c.CloseNow()
	h.ctrl.Stop()
	ce := expectClose(t, c, websocket.StatusGoingAway)
	if !strings.Contains(ce.Reason, "shutting down") {
		t.Errorf("reason = %q", ce.Reason)
	}
	// A new upgrade after Stop is refused, not accepted.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http")+"/ws", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial after Stop: err=%v resp=%v", err, resp)
	}
}

func TestPerTenantCapRefuses503(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.WebsocketMaxConnsPerTenant = 1 }, Accept{})
	c := h.dial(t)
	defer c.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http")+"/ws", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("second dial: err=%v resp=%v", err, resp)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("503 without Retry-After")
	}
	// Closing the first frees the slot.
	_ = c.Close(websocket.StatusNormalClosure, "")
	deadline := time.Now().Add(2 * time.Second)
	for h.ctrl.Count() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	c2 := h.dial(t)
	c2.CloseNow()
}

func TestSubprotocolNegotiation(t *testing.T) {
	h := newHarness(t, nil, Accept{Subprotocols: []string{"pony.v1"}, SubprotocolRequired: true})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws"
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{"other"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unacceptable subprotocol: err=%v resp=%v", err, resp)
	}
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: []string{"chat", "pony.v1"}})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	if got := c.Subprotocol(); got != "pony.v1" {
		t.Fatalf("negotiated %q", got)
	}
	writeText(t, c, "x")
	if got := gjson.Get(h.waitEnvelopes(t, 1)[0].Payload.Raw, "_txc.websocket.session.subprotocol").String(); got != "pony.v1" {
		t.Fatalf("envelope subprotocol = %q", got)
	}
}

func TestOriginPolicy(t *testing.T) {
	h := newHarness(t, nil, Accept{})
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Default: a cross-origin browser is refused (403) …
	hdr := http.Header{"Origin": []string{"https://evil.example"}}
	_, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: err=%v resp=%v", err, resp)
	}
	// … same-host and no-Origin (non-browser) are accepted …
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Origin": []string{h.srv.URL}}})
	if err != nil {
		t.Fatalf("same-origin dial: %v", err)
	}
	c.CloseNow()
	// … and a stack may open it.
	h.setAccept(Accept{AnyOrigin: true})
	c, _, err = websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("any-origin dial: %v", err)
	}
	c.CloseNow()
}

func TestAdmissionDeniedResults(t *testing.T) {
	h := newHarness(t, nil, Accept{})
	denied := func(status int, reason string) event.Payload {
		return event.Payload{Raw: `{"_txc":{"admission":{"denied":true,"status":` + itoa(status) + `,"reason":"` + reason + `"}}}`, Type: event.JSON}
	}
	// 429: the message is dropped, the socket lives.
	h.setReply(func(*event.Envelope) event.Payload { return denied(429, "rate_limited") })
	c := h.dial(t)
	defer c.CloseNow()
	writeText(t, c, "a")
	h.waitEnvelopes(t, 1)
	h.setReply(func(env *event.Envelope) event.Payload {
		sid := gjson.Get(env.Payload.Raw, "_txc.websocket.session.id").String()
		_ = h.ctrl.Send(context.Background(), "acme", sid, MessageText, []byte("still here"))
		return event.Payload{Raw: `{}`, Type: event.JSON}
	})
	writeText(t, c, "b")
	if got := readText(t, c); got != "still here" {
		t.Fatalf("after 429: %q", got)
	}
	// 403: policy close.
	h.setReply(func(*event.Envelope) event.Payload { return denied(403, "suspended") })
	writeText(t, c, "c")
	expectClose(t, c, websocket.StatusPolicyViolation)

	// 503: try again later.
	h.setReply(func(*event.Envelope) event.Payload { return denied(503, "draining") })
	c2 := h.dial(t)
	defer c2.CloseNow()
	writeText(t, c2, "d")
	expectClose(t, c2, websocket.StatusTryAgainLater)
}

func TestStreamedResultIsDrained(t *testing.T) {
	h := newHarness(t, nil, Accept{})
	h.setReply(func(env *event.Envelope) event.Payload {
		env.ResCh <- event.Payload{Raw: `{"_txc":{"web":{"res":{"status":200}}}}`, Type: event.StreamHead}
		env.ResCh <- event.Payload{Raw: "chunk", Type: event.StreamChunk}
		return event.Payload{Type: event.StreamEnd}
	})
	c := h.dial(t)
	defer c.CloseNow()
	writeText(t, c, "stream")
	h.waitEnvelopes(t, 1)
	// The session is intact: a normal reply still works.
	h.setReply(func(env *event.Envelope) event.Payload {
		sid := gjson.Get(env.Payload.Raw, "_txc.websocket.session.id").String()
		_ = h.ctrl.Send(context.Background(), "acme", sid, MessageBinary, []byte{1, 2, 3})
		return event.Payload{Raw: `{}`, Type: event.JSON}
	})
	writeText(t, c, "next")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || len(data) != 3 {
		t.Fatalf("binary reply: typ=%v data=%v err=%v", typ, data, err)
	}
}

func TestCloseSessionByStack(t *testing.T) {
	h := newHarness(t, nil, Accept{})
	c := h.dial(t)
	defer c.CloseNow()
	writeText(t, c, "x")
	sid := gjson.Get(h.waitEnvelopes(t, 1)[0].Payload.Raw, "_txc.websocket.session.id").String()
	if err := h.ctrl.CloseSession(context.Background(), "acme", sid, 4000, "done"); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	ce := expectClose(t, c, websocket.StatusCode(4000))
	if ce.Reason != "done" {
		t.Errorf("reason = %q", ce.Reason)
	}
	if err := h.ctrl.CloseSession(context.Background(), "acme", sid, 1000, ""); !errors.Is(err, ErrSessionNotFound) && !errors.Is(err, ErrSessionClosed) {
		t.Errorf("second close err = %v", err)
	}
}

func TestRunTimeoutKeepsSession(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.WebsocketRunTimeout = "100ms" }, Accept{})
	release := make(chan struct{})
	h.setReply(func(*event.Envelope) event.Payload {
		<-release
		return event.Payload{Raw: `{}`, Type: event.JSON}
	})
	c := h.dial(t)
	defer c.CloseNow()
	writeText(t, c, "slow")
	time.Sleep(250 * time.Millisecond)
	close(release)
	h.setReply(func(env *event.Envelope) event.Payload {
		sid := gjson.Get(env.Payload.Raw, "_txc.websocket.session.id").String()
		_ = h.ctrl.Send(context.Background(), "acme", sid, MessageText, []byte("alive"))
		return event.Payload{Raw: `{}`, Type: event.JSON}
	})
	writeText(t, c, "fast")
	if got := readText(t, c); got != "alive" {
		t.Fatalf("after timeout: %q", got)
	}
}

func TestDisabledControllerIsInert(t *testing.T) {
	pu := &processor.Unit{Conf: config.Config{Personalities: "web"}, Logger: zap.NewNop()}
	c := NewController(context.Background(), pu)
	if c.Enabled() {
		t.Fatal("enabled without the personality")
	}
	if err := c.RecordAccept("ws_x", Accept{Tenant: "acme"}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("RecordAccept err = %v", err)
	}
	if _, ok := c.Claim("ws_x"); ok {
		t.Fatal("claim succeeded on a disabled controller")
	}
	if err := c.Send(context.Background(), "acme", "ws_x", MessageText, nil); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Send err = %v", err)
	}
	c.Start()
	c.Stop()
}

func TestPendingAcceptExpires(t *testing.T) {
	pu := &processor.Unit{Conf: config.Config{Personalities: "web,websocket"}, Logger: zap.NewNop()}
	c := NewController(context.Background(), pu)
	now := time.Now()
	c.now = func() time.Time { return now }
	if err := c.RecordAccept("ws_a", Accept{Tenant: "acme"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(acceptTTL + time.Second)
	c.sweepPending(now)
	if _, ok := c.Claim("ws_a"); ok {
		t.Fatal("expired accept was claimable")
	}
	if err := c.RecordAccept("ws_b", Accept{Tenant: "acme", IdleTimeout: 48 * time.Hour, MaxMessageBytes: 1 << 40}); err != nil {
		t.Fatal(err)
	}
	a, ok := c.Claim("ws_b")
	if !ok {
		t.Fatal("claim")
	}
	if a.IdleTimeout != c.lim.maxIdleTimeout || a.MaxMessageBytes != c.lim.maxMessageBytes {
		t.Fatalf("overrides not clamped: %v %d", a.IdleTimeout, a.MaxMessageBytes)
	}
	if _, ok := c.Claim("ws_b"); ok {
		t.Fatal("claim is not single-use")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
