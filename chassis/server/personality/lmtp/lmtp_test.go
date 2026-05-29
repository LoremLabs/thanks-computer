package lmtp

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// routeAllResolver routes every rcpt to a single (tenant, stack).
// Used by tests that don't care about routing and just want one
// envelope per delivery (regression coverage for the Phase 0/1/3
// shape, where the resolver was implicit).
type routeAllResolver struct {
	tenant, stack string
}

func (r routeAllResolver) ResolveRecipient(rcpt, _ string) (ingress.RouteTarget, bool) {
	return ingress.RouteTarget{
		Tenant:   r.tenant,
		Stack:    r.stack,
		Ingress:  rcpt,
		Verified: true,
	}, true
}

// routeByDomainResolver routes rcpts per their `@<domain>` to
// different (tenant, stack) pairs. Used by the cross-tenant
// grouping test.
type routeByDomainResolver map[string]ingress.RouteTarget // key = "@<domain>"

func (r routeByDomainResolver) ResolveRecipient(rcpt, _ string) (ingress.RouteTarget, bool) {
	at := -1
	for i := len(rcpt) - 1; i >= 0; i-- {
		if rcpt[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 {
		return ingress.RouteTarget{}, false
	}
	t, ok := r["@"+rcpt[at+1:]]
	if !ok {
		return ingress.RouteTarget{}, false
	}
	t.Ingress = rcpt
	t.Verified = true
	return t, true
}

// fakeResponder reads envelopes off the bus, asserts the source is
// "lmtp", optionally inspects the envelope via `inspect`, and writes
// `response` back to ResCh. Use to drive Session.Data end-to-end
// without standing up the full processor pipeline.
type fakeResponder struct {
	response string
	inspect  func(t *testing.T, envelope *event.Envelope)
}

func startFakeProcessor(t *testing.T, bus <-chan *event.Envelope, fr fakeResponder) {
	t.Helper()
	go func() {
		for env := range bus {
			if env == nil {
				return
			}
			if env.Src != "lmtp" {
				t.Errorf("envelope.Src = %q, want \"lmtp\"", env.Src)
			}
			if fr.inspect != nil {
				fr.inspect(t, env)
			}
			env.ResCh <- event.Payload{Raw: fr.response, Type: event.JSON}
		}
	}()
}

// startTestController spins up an LMTP controller bound to a TCP
// loopback ephemeral port, with a fake processor wired to bus.
// Returns the dial address and a cleanup func.
//
// TCP-loopback (not Unix socket) for portability: macOS limits Unix
// `sun_path` to 104 chars and Go's t.TempDir nested-subtest paths
// (`/var/folders/.../<TestName>/<subtest>/001`) routinely exceed
// that — bind fails with "invalid argument". TCP loopback sidesteps
// the limit and matches what's tested in CI.
func startTestController(t *testing.T, fr fakeResponder) (addr string, stop func()) {
	t.Helper()

	bus := make(chan *event.Envelope, 4)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities:     "lmtp",
			LMTPListenAddrs:   []string{"127.0.0.1:0"}, // ephemeral port
			LMTPMaxMsgBytes:   26214400,
			LMTPMaxRecipients: 50,
			LMTPReadTimeout:   "30s",
			LMTPDataTimeout:   "60s",
			LMTPRespTimeout:   "5s",
			LMTPHostname:      "test.localhost",
		},
		Logger: testLogger(t),
		Bus:    bus,
	}

	ctx, cancel := context.WithCancel(context.Background())
	// routeAll: route every rcpt to a single test tenant/stack so the
	// per-rcpt grouping yields one envelope. Tests that need
	// cross-tenant grouping use a different resolver.
	ctrl := NewController(ctx, pu, routeAllResolver{tenant: "testtenant", stack: "testtenant/inbox"})

	startFakeProcessor(t, bus, fr)
	ctrl.Start()

	// Recover the bound port. The Server's stored Addr is what was
	// requested (":0"), so read the actual port off the bound
	// listener instead.
	if len(ctrl.servers) == 0 || ctrl.servers[0] == nil {
		t.Fatal("controller did not bind a server")
	}
	// The smtp.Server doesn't expose its listener directly; we recorded
	// the listener ourselves in lmtp.go via Server.Addr after binding,
	// but the test needs the real bound port. Use the server's first
	// listener via reflection on the unexported field is brittle;
	// simpler: ask the controller for the address it actually bound.
	addr = ctrl.boundAddrs()[0]

	// Brief wait for the listener to be ready. The Start() above does
	// the bind synchronously before returning; the goroutine that calls
	// Serve runs after. A tiny grace period avoids a connect race on
	// slower CI without a polling loop.
	time.Sleep(20 * time.Millisecond)

	return addr, func() {
		ctrl.Stop()
		cancel()
	}
}

// startSilentController spins up an LMTP controller wired to a bus
// that drains envelopes but never responds. Forces the session to
// hit LMTPRespTimeout — useful for testing the chassis-timeout
// transport path (451). `respTimeout` controls how long sessions
// wait before timing out.
func startSilentController(t *testing.T, respTimeout string) (addr string, stop func()) {
	t.Helper()

	bus := make(chan *event.Envelope, 1)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities:     "lmtp",
			LMTPListenAddrs:   []string{"127.0.0.1:0"},
			LMTPMaxMsgBytes:   26214400,
			LMTPMaxRecipients: 50,
			LMTPReadTimeout:   "30s",
			LMTPDataTimeout:   "60s",
			LMTPRespTimeout:   respTimeout,
			LMTPHostname:      "test",
		},
		Logger: testLogger(t),
		Bus:    bus,
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Route everything to a single test stack so dispatch actually
	// happens and hits the silent bus, exercising the timeout path.
	ctrl := NewController(ctx, pu, routeAllResolver{tenant: "testtenant", stack: "testtenant/inbox"})
	ctrl.Start()

	go func() {
		for env := range bus {
			if env == nil {
				return
			}
			// Deliberately do not write to env.ResCh.
			_ = env
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if len(ctrl.boundAddrs()) == 0 {
		t.Fatal("controller did not bind")
	}
	addr = ctrl.boundAddrs()[0]
	return addr, func() {
		ctrl.Stop()
		cancel()
	}
}

// dialLMTP returns a connected LMTP client over the given TCP address.
func dialLMTP(t *testing.T, addr string) *smtp.Client {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial tcp %s: %v", addr, err)
	}
	client := smtp.NewClientLMTP(conn)
	if err := client.Hello("test"); err != nil {
		_ = client.Close()
		t.Fatalf("LHLO: %v", err)
	}
	return client
}

// sendMail walks a single LMTP transaction. On success the server's
// status line was 2xx; on failure the returned err is an *smtp.SMTPError
// the caller can inspect.
func sendMail(client *smtp.Client, from string, to []string, body string) error {
	return client.SendMail(from, to, strings.NewReader(body))
}

// codeOf extracts the SMTP code from an LMTP/SMTP client error.
//
// LMTP wraps per-recipient verdicts in `smtp.LMTPDataError`
// (map[rcpt]*SMTPError); plain SMTP returns a bare *SMTPError. We
// handle both. When the LMTPDataError has multiple recipients all
// errored, the first non-2xx code is returned — broadcast cases use
// this; per-recipient inspection uses `codeFor`.
func codeOf(err error) int {
	if err == nil {
		return 250
	}
	if se, ok := err.(*smtp.SMTPError); ok {
		return se.Code
	}
	if le, ok := err.(smtp.LMTPDataError); ok {
		for _, se := range le {
			if se != nil {
				return se.Code
			}
		}
	}
	return 0
}

// codeFor returns the per-recipient code from an LMTPDataError.
//
// LMTPDataError is sparse: it only contains the *failed* recipients.
// Any rcpt absent from the map succeeded (2xx). So:
//   - err == nil                       → all rcpts 2xx                → 250
//   - err is LMTPDataError, rcpt in    → that rcpt's *SMTPError code  → e.g. 550
//   - err is LMTPDataError, rcpt out   → that rcpt succeeded          → 250
//   - err is bare *SMTPError           → whole tx failed              → that code
func codeFor(err error, rcpt string) int {
	if err == nil {
		return 250
	}
	if le, ok := err.(smtp.LMTPDataError); ok {
		se, present := le[rcpt]
		if !present {
			return 250 // not in failure map → succeeded
		}
		if se == nil {
			return 250
		}
		return se.Code
	}
	if se, ok := err.(*smtp.SMTPError); ok {
		// Bare *SMTPError happens when the entire transaction
		// failed before per-rcpt delivery (oversize, timeout); the
		// same code applies to every rcpt.
		return se.Code
	}
	return 0
}

func TestData_DefaultDeny(t *testing.T) {
	// Pipeline returns "{}" — no _txc.lmtp.res.code set. Expect 550.
	addr, stop := startTestController(t, fakeResponder{response: "{}"})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	err := sendMail(client, "alice@example.com", []string{"bob@your.tenant"}, "hello")
	if got := codeOf(err); got != 550 {
		t.Errorf("default-deny: got code %d, want 550", got)
	}
}

func TestData_Accept(t *testing.T) {
	resp := mustSet("{}", "_txc.lmtp.res.code", 250)
	addr, stop := startTestController(t, fakeResponder{response: resp})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	err := sendMail(client, "alice@example.com", []string{"bob@your.tenant"}, "hello")
	if err != nil {
		t.Errorf("accept: unexpected err: %v (code=%d)", err, codeOf(err))
	}
}

func TestData_RejectAndDefer(t *testing.T) {
	cases := []struct {
		name string
		code int
	}{
		{"reject_550", 550},
		{"defer_451", 451},
		{"reject_554", 554},
		{"mailbox_full_452", 452},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustSet("{}", "_txc.lmtp.res.code", tc.code)
			resp = mustSet(resp, "_txc.lmtp.res.msg", "test")
			addr, stop := startTestController(t, fakeResponder{response: resp})
			defer stop()

			client := dialLMTP(t, addr)
			defer client.Close()

			err := sendMail(client, "alice@example.com", []string{"bob@your.tenant"}, "hello")
			if got := codeOf(err); got != tc.code {
				t.Errorf("got code %d, want %d (err=%v)", got, tc.code, err)
			}
		})
	}
}

func TestData_EnvelopeShape(t *testing.T) {
	var seenRaw string
	fr := fakeResponder{
		response: mustSet("{}", "_txc.lmtp.res.code", 250),
		inspect: func(t *testing.T, env *event.Envelope) {
			seenRaw = env.Payload.Raw
		},
	}
	addr, stop := startTestController(t, fr)
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	body := "Subject: hi\r\n\r\nhello world\r\n"
	if err := sendMail(client,
		"alice@example.com",
		[]string{"bob@your.tenant", "carol@your.tenant"},
		body); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Give the inspect closure a brief moment in case the test sees
	// the response before the assignment finishes (it won't in
	// practice because fakeResponder runs synchronously inside the
	// goroutine reading the bus, but a 20ms grace is cheap).
	time.Sleep(20 * time.Millisecond)

	if seenRaw == "" {
		t.Fatal("no envelope captured")
	}
	if got := gjson.Get(seenRaw, "_txc.src").String(); got != "lmtp" {
		t.Errorf("_txc.src = %q, want \"lmtp\"", got)
	}
	if got := gjson.Get(seenRaw, "_txc.lmtp.listener").String(); got != "default" {
		t.Errorf("_txc.lmtp.listener = %q, want \"default\"", got)
	}
	if got := gjson.Get(seenRaw, "_txc.lmtp.mail.from").String(); got != "alice@example.com" {
		t.Errorf("mail.from = %q", got)
	}
	rcpt := gjson.Get(seenRaw, "_txc.lmtp.rcpt").Array()
	if len(rcpt) != 2 ||
		rcpt[0].String() != "bob@your.tenant" ||
		rcpt[1].String() != "carol@your.tenant" {
		t.Errorf("rcpt = %v", rcpt)
	}
	if gjson.Get(seenRaw, "_txc.lmtp.msg.raw").String() == "" {
		t.Error("msg.raw is empty; expected b64 body")
	}
	if got := gjson.Get(seenRaw, "_txc.rid").String(); got == "" {
		t.Error("_txc.rid is empty")
	}
	if got := gjson.Get(seenRaw, "_ts").String(); got == "" {
		t.Error("_ts is empty")
	}
}

func TestData_PipelineTimeout(t *testing.T) {
	addr, stop := startSilentController(t, "100ms")
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	err := sendMail(client, "alice@example.com", []string{"bob@your.tenant"}, "hi")
	if got := codeOf(err); got != 451 {
		t.Errorf("timeout: got code %d, want 451 (err=%v)", got, err)
	}
}

// Helper tests — pure functions, no I/O.

func TestParseListenAddr(t *testing.T) {
	cases := []struct {
		in      string
		network string
		bind    string
	}{
		{"unix:/tmp/x.sock", "unix", "/tmp/x.sock"},
		{"tcp::5050", "tcp", ":5050"},
		{":5050", "tcp", ":5050"},
		{"127.0.0.1:5050", "tcp", "127.0.0.1:5050"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotN, gotB := parseListenAddr(tc.in)
			if gotN != tc.network || gotB != tc.bind {
				t.Errorf("parseListenAddr(%q) = (%q,%q), want (%q,%q)",
					tc.in, gotN, gotB, tc.network, tc.bind)
			}
		})
	}
}

func TestBroadcastVerdict(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantCode int
		wantNil  bool
	}{
		{"empty", "", 550, false},
		{"no_code", "{}", 550, false},
		{"explicit_550", mustSet("{}", "_txc.lmtp.res.code", 550), 550, false},
		{"explicit_250", mustSet("{}", "_txc.lmtp.res.code", 250), 0, true},
		{"explicit_451", mustSet("{}", "_txc.lmtp.res.code", 451), 451, false},
		{"explicit_552", mustSet("{}", "_txc.lmtp.res.code", 552), 552, false},
		{"invalid_json", "{not json", 550, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := broadcastVerdict(tc.raw)
			if tc.wantNil {
				if err != nil {
					t.Errorf("want nil, got %v", err)
				}
				return
			}
			if got := codeOf(err); got != tc.wantCode {
				t.Errorf("got %d, want %d", got, tc.wantCode)
			}
		})
	}
}

func TestNonEmpty(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{nil, nil},
		{[]string{}, []string{}},
		{[]string{""}, []string{}},
		{[]string{"", " ", "\t"}, []string{}},
		{[]string{"a", "", "b"}, []string{"a", "b"}},
	}
	for i, tc := range cases {
		got := nonEmpty(append([]string(nil), tc.in...))
		if len(got) != len(tc.want) {
			t.Errorf("case %d: got %v, want %v", i, got, tc.want)
			continue
		}
		for j := range got {
			if got[j] != tc.want[j] {
				t.Errorf("case %d: got %v, want %v", i, got, tc.want)
				break
			}
		}
	}
}

// testLogger returns a zap logger that pipes through t.Logf so panics
// and Fatal-level logs (which zap.NewNop swallows silently) surface in
// the test output. Crucial for diagnosing bind failures — pu.Logger.Fatal
// in lmtp.go would otherwise be a no-op with NopLogger.
func testLogger(t *testing.T) *zap.Logger {
	t.Helper()
	return zap.New(zapcore.NewCore(
		zapcore.NewConsoleEncoder(zap.NewDevelopmentEncoderConfig()),
		zapcore.AddSync(testWriter{t: t}),
		zap.DebugLevel,
	))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// mustSet is a test helper that panics on sjson errors so table cases
// stay terse. Equivalent to assert-and-return.
func mustSet(json string, path string, val interface{}) string {
	out, err := sjson.Set(json, path, val)
	if err != nil {
		panic(fmt.Sprintf("sjson.Set(%q, %q, %v): %v", json, path, val, err))
	}
	return out
}
