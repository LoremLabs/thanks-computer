package lmtp

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// startGroupingController is a more flexible variant of
// startTestController that:
//   - takes a custom MailResolver
//   - lets the test inspect every envelope on the bus (not just one)
//   - returns one response per envelope from a queue
//
// The fakeResponder shape can't model this because it only knows a
// single response.
func startGroupingController(
	t *testing.T,
	resolver ingress.MailResolver,
	responses map[string]string, // tenant → response JSON (for inspection)
) (addr string, captured *capturedEnvelopes, stop func()) {
	t.Helper()

	bus := make(chan *event.Envelope, 8)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities:     "lmtp",
			LMTPListenAddrs:   []string{"127.0.0.1:0"},
			LMTPMaxMsgBytes:   26214400,
			LMTPMaxRecipients: 50,
			LMTPReadTimeout:   "30s",
			LMTPDataTimeout:   "60s",
			LMTPRespTimeout:   "5s",
			LMTPHostname:      "test",
		},
		Logger: testLogger(t),
		Bus:    bus,
	}

	captured = &capturedEnvelopes{}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu, resolver)
	ctrl.Start()

	go func() {
		for env := range bus {
			if env == nil {
				return
			}
			tenant := gjson.Get(env.Payload.Raw, "_txc.route.tenant").String()
			captured.add(env.Payload.Raw)
			resp, ok := responses[tenant]
			if !ok {
				resp = "{}"
			}
			env.ResCh <- event.Payload{Raw: resp, Type: event.JSON}
		}
	}()

	time.Sleep(20 * time.Millisecond)
	if len(ctrl.boundAddrs()) == 0 {
		t.Fatal("controller did not bind")
	}
	addr = ctrl.boundAddrs()[0]
	stop = func() {
		ctrl.Stop()
		cancel()
	}
	return
}

type capturedEnvelopes struct {
	mu   sync.Mutex
	raws []string
}

func (c *capturedEnvelopes) add(raw string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raws = append(c.raws, raw)
}

func (c *capturedEnvelopes) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.raws))
	copy(out, c.raws)
	return out
}

// TestLMTPGrouping_CrossTenant — one DATA, four RCPTs split across
// two tenants + one unrouted. Expectation:
//   - Two envelopes dispatched (one per tenant).
//   - rcpt[3] (unrouted) gets 550 without any envelope.
//   - Each envelope's `_txc.lmtp.rcpt` is its group sublist.
//   - Each envelope's `_txc.lmtp.transaction_rcpt` is the full list.
//   - Per-rcpt verdicts stitch back to original RCPT TO order.
func TestLMTPGrouping_CrossTenant(t *testing.T) {
	resolver := routeByDomainResolver{
		"@acme.example": ingress.RouteTarget{Tenant: "acme", Stack: "acme/inbox"},
		"@beta.example": ingress.RouteTarget{Tenant: "beta", Stack: "beta/inbox"},
	}

	// Each tenant's stack accepts all its rcpts via broadcast.
	responses := map[string]string{
		"acme": `{"_txc":{"lmtp":{"res":{"code":250,"msg":"acme accepted"}}}}`,
		"beta": `{"_txc":{"lmtp":{"res":{"code":250,"msg":"beta accepted"}}}}`,
	}

	addr, captured, stop := startGroupingController(t, resolver, responses)
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	rcpts := []string{
		"alice@acme.example",   // → acme
		"bob@beta.example",     // → beta
		"carol@acme.example",   // → acme (groups with alice)
		"nobody@unknown.example", // unrouted → 550 directly
	}
	err := client.SendMail("sender@out.example", rcpts, strings.NewReader("hi"))

	// Check per-rcpt verdicts.
	if got := codeFor(err, "alice@acme.example"); got != 250 {
		t.Errorf("alice = %d, want 250", got)
	}
	if got := codeFor(err, "bob@beta.example"); got != 250 {
		t.Errorf("bob = %d, want 250", got)
	}
	if got := codeFor(err, "carol@acme.example"); got != 250 {
		t.Errorf("carol = %d, want 250", got)
	}
	if got := codeFor(err, "nobody@unknown.example"); got != 550 {
		t.Errorf("nobody = %d, want 550 (unrouted default-deny)", got)
	}

	// Check envelope grouping.
	envelopes := captured.snapshot()
	if len(envelopes) != 2 {
		t.Fatalf("got %d envelopes, want 2 (one per tenant group)", len(envelopes))
	}

	// Find each group by tenant.
	groups := map[string]string{}
	for _, raw := range envelopes {
		tenant := gjson.Get(raw, "_txc.route.tenant").String()
		groups[tenant] = raw
	}
	if _, ok := groups["acme"]; !ok {
		t.Fatal("no acme envelope")
	}
	if _, ok := groups["beta"]; !ok {
		t.Fatal("no beta envelope")
	}

	// acme envelope carries alice + carol in its rcpt sublist.
	acmeRcpts := gjson.Get(groups["acme"], "_txc.lmtp.rcpt").Array()
	if len(acmeRcpts) != 2 ||
		!containsRcpt(acmeRcpts, "alice@acme.example") ||
		!containsRcpt(acmeRcpts, "carol@acme.example") {
		t.Errorf("acme group rcpt = %v, want [alice, carol]", acmeRcpts)
	}

	// beta envelope carries just bob.
	betaRcpts := gjson.Get(groups["beta"], "_txc.lmtp.rcpt").Array()
	if len(betaRcpts) != 1 || betaRcpts[0].String() != "bob@beta.example" {
		t.Errorf("beta group rcpt = %v, want [bob]", betaRcpts)
	}

	// Both envelopes carry the full transaction list in
	// _txc.lmtp.transaction_rcpt.
	for tenant, raw := range groups {
		txRcpts := gjson.Get(raw, "_txc.lmtp.transaction_rcpt").Array()
		if len(txRcpts) != 4 {
			t.Errorf("%s transaction_rcpt len = %d, want 4 (full list, "+
				"including the unrouted rcpt)", tenant, len(txRcpts))
		}
	}

	// Both envelopes carry pre-stamped _txc.route.*. detectTenantBody
	// must respect this and not re-resolve.
	for tenant, raw := range groups {
		if got := gjson.Get(raw, "_txc.route.to").String(); got == "" {
			t.Errorf("%s envelope missing _txc.route.to (boot pipeline "+
				"would re-resolve and miss)", tenant)
		}
		wantStack := tenant + "/inbox"
		if got := gjson.Get(raw, "_txc.route.stack").String(); got != wantStack {
			t.Errorf("%s _txc.route.stack = %q, want %q", tenant, got, wantStack)
		}
	}
}

// TestLMTPGrouping_PerGroupVerdicts — within a multi-tenant delivery,
// each tenant returns its OWN per-recipient verdicts. The group's
// `_txc.lmtp.res.recipients` is indexed within the group's sublist
// (NOT the original transaction order); the inlet stitches them
// back via the rcpt string.
func TestLMTPGrouping_PerGroupVerdicts(t *testing.T) {
	resolver := routeByDomainResolver{
		"@acme.example": ingress.RouteTarget{Tenant: "acme", Stack: "acme/inbox"},
		"@beta.example": ingress.RouteTarget{Tenant: "beta", Stack: "beta/inbox"},
	}
	// acme accepts both its rcpts (250 + 250); beta accepts the first
	// and rejects the second (within beta's own rcpt sublist, that's
	// indices 0 and 1).
	responses := map[string]string{
		"acme": `{"_txc":{"lmtp":{"res":{"recipients":[{"code":250},{"code":250}]}}}}`,
		"beta": `{"_txc":{"lmtp":{"res":{"recipients":[{"code":250},{"code":550,"msg":"no inbox"}]}}}}`,
	}

	addr, _, stop := startGroupingController(t, resolver, responses)
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	rcpts := []string{
		"alice@acme.example",   // acme[0]
		"bob@beta.example",     // beta[0]
		"carol@acme.example",   // acme[1]
		"dan@beta.example",     // beta[1] ← rejected
	}
	err := client.SendMail("sender@out.example", rcpts, strings.NewReader("hi"))

	if got := codeFor(err, "alice@acme.example"); got != 250 {
		t.Errorf("alice = %d, want 250", got)
	}
	if got := codeFor(err, "bob@beta.example"); got != 250 {
		t.Errorf("bob = %d, want 250", got)
	}
	if got := codeFor(err, "carol@acme.example"); got != 250 {
		t.Errorf("carol = %d, want 250", got)
	}
	if got := codeFor(err, "dan@beta.example"); got != 550 {
		t.Errorf("dan = %d, want 550 (beta rejected this rcpt within its group)", got)
	}
}

// TestLMTPGrouping_AllUnrouted — nil resolver, every rcpt 550s
// without any envelope dispatched.
func TestLMTPGrouping_AllUnrouted(t *testing.T) {
	addr, captured, stop := startGroupingController(t, nilResolver{}, nil)
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	rcpts := []string{"a@x", "b@y", "c@z"}
	err := client.SendMail("sender@out.example", rcpts, strings.NewReader("hi"))

	for _, r := range rcpts {
		if got := codeFor(err, r); got != 550 {
			t.Errorf("%s = %d, want 550", r, got)
		}
	}
	if envs := captured.snapshot(); len(envs) != 0 {
		t.Errorf("got %d envelopes dispatched, want 0 (all unrouted)", len(envs))
	}
}

// TestLMTPGrouping_OneFailingGroupDoesNotPoisonOthers — if one group's
// dispatch times out (or otherwise transport-fails), the OTHER
// groups' verdicts must still arrive correctly.
func TestLMTPGrouping_OneFailingGroupDoesNotPoisonOthers(t *testing.T) {
	resolver := routeByDomainResolver{
		"@acme.example":  ingress.RouteTarget{Tenant: "acme", Stack: "acme/inbox"},
		"@stall.example": ingress.RouteTarget{Tenant: "stall", Stack: "stall/inbox"},
	}

	bus := make(chan *event.Envelope, 4)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities:     "lmtp",
			LMTPListenAddrs:   []string{"127.0.0.1:0"},
			LMTPMaxMsgBytes:   26214400,
			LMTPMaxRecipients: 50,
			LMTPRespTimeout:   "200ms",
			LMTPHostname:      "test",
		},
		Logger: testLogger(t),
		Bus:    bus,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctrl := NewController(ctx, pu, resolver)
	ctrl.Start()
	defer ctrl.Stop()

	// Acme responds quickly; stall never responds (forces 451 for stall's rcpts).
	go func() {
		for env := range bus {
			if env == nil {
				return
			}
			tenant := gjson.Get(env.Payload.Raw, "_txc.route.tenant").String()
			if tenant == "stall" {
				continue // intentionally don't respond
			}
			env.ResCh <- event.Payload{
				Raw:  `{"_txc":{"lmtp":{"res":{"code":250}}}}`,
				Type: event.JSON,
			}
		}
	}()

	time.Sleep(20 * time.Millisecond)
	client := dialLMTP(t, ctrl.boundAddrs()[0])
	defer client.Close()

	err := client.SendMail("sender@out.example",
		[]string{"alice@acme.example", "bob@stall.example"},
		strings.NewReader("hi"))

	if got := codeFor(err, "alice@acme.example"); got != 250 {
		t.Errorf("alice (acme, responding) = %d, want 250", got)
	}
	if got := codeFor(err, "bob@stall.example"); got != 451 {
		t.Errorf("bob (stall, timed out) = %d, want 451 (per-group failure, not poisoning alice)", got)
	}
}

// nilResolver is a MailResolver that never matches — every rcpt
// goes unrouted.
type nilResolver struct{}

func (nilResolver) ResolveRecipient(_, _ string) (ingress.RouteTarget, bool) {
	return ingress.RouteTarget{}, false
}

func containsRcpt(arr []gjson.Result, rcpt string) bool {
	for _, r := range arr {
		if r.String() == rcpt {
			return true
		}
	}
	return false
}

// Make sure these don't trigger unused-import errors when the cross-
// tenant test isn't running stand-alone.
var (
	_ = net.Dial
	_ = zap.NewNop
	_ = smtp.NewClientLMTP
)
