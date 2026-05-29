package lmtp

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// strategyAOnlyResolver is the actual yamlResolver constructed from
// WithDefaultMailHosts — bridges the chassis-side resolver to the
// LMTP head's MailResolver expectation so we exercise the real parse
// path, not a hand-rolled stub.
func strategyAOnlyResolver(t *testing.T, hosts []string) ingress.MailResolver {
	t.Helper()
	r, err := ingress.LoadResolverFromFile("", ingress.WithDefaultMailHosts(hosts))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil resolver")
	}
	mr, ok := r.(ingress.MailResolver)
	if !ok {
		t.Fatal("resolver does not satisfy MailResolver")
	}
	return mr
}

// TestStrategyA_EndToEnd — drive an LMTP transaction whose rcpt is
// `acme.support+monday@chassis.example`. Verify:
//   - the envelope dispatched to the bus carries `_txc.route.stack`
//     = `acme/support`
//   - the per-recipient verdict round-trips correctly
//   - `_txc.lmtp.rcpt[0]` is the unmodified address (including `+monday`)
//     so rules can re-parse for the modifier if they need it
func TestStrategyA_EndToEnd(t *testing.T) {
	resolver := strategyAOnlyResolver(t, []string{"chassis.example"})

	addr, captured, stop := startGroupingController(t, resolver, map[string]string{
		"acme": `{"_txc":{"lmtp":{"res":{"code":250}}}}`,
	})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	err := client.SendMail(
		"sender@out.example",
		[]string{"acme.support+monday@chassis.example"},
		strings.NewReader("hi"))
	if got := codeFor(err, "acme.support+monday@chassis.example"); got != 250 {
		t.Errorf("verdict = %d, want 250 (err=%v)", got, err)
	}

	envs := captured.snapshot()
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes, want 1", len(envs))
	}
	env := envs[0]

	if got := gjson.Get(env, "_txc.route.tenant").String(); got != "acme" {
		t.Errorf("_txc.route.tenant = %q, want %q", got, "acme")
	}
	if got := gjson.Get(env, "_txc.route.stack").String(); got != "acme/support" {
		t.Errorf("_txc.route.stack = %q, want %q", got, "acme/support")
	}
	// rcpt list carries the full original address (with +monday) —
	// the source of truth for rules that want the modifier.
	rcpts := gjson.Get(env, "_txc.lmtp.rcpt").Array()
	if len(rcpts) != 1 || rcpts[0].String() != "acme.support+monday@chassis.example" {
		t.Errorf("_txc.lmtp.rcpt = %v, want [acme.support+monday@chassis.example]", rcpts)
	}
	// The chassis must NOT stamp a derived `address.modifier` field —
	// any exemplar would be lossy when rcpts in a group carry
	// different modifiers, and the truth is in rcpt[i] anyway.
	if gjson.Get(env, "_txc.lmtp.address").Exists() {
		t.Errorf("_txc.lmtp.address unexpectedly stamped (modifier should not be on envelope; rules parse from rcpt[i])")
	}
}

// TestStrategyA_SameTenantStackBatches — two rcpts with the SAME
// tenant.stack but different modifiers must group into one envelope
// (modifier doesn't affect routing). Both rcpts present in the
// envelope's rcpt sublist with their `+modifier` suffixes intact —
// rules read them from there if they need per-rcpt detail.
func TestStrategyA_SameTenantStackBatches(t *testing.T) {
	resolver := strategyAOnlyResolver(t, []string{"chassis.example"})

	addr, captured, stop := startGroupingController(t, resolver, map[string]string{
		"acme": `{"_txc":{"lmtp":{"res":{"code":250}}}}`,
	})
	defer stop()

	client := dialLMTP(t, addr)
	defer client.Close()

	if err := client.SendMail(
		"sender@out.example",
		[]string{
			"acme.support+monday@chassis.example",
			"acme.support+tuesday@chassis.example",
		},
		strings.NewReader("hi")); err != nil {
		t.Fatalf("send: %v", err)
	}

	envs := captured.snapshot()
	if len(envs) != 1 {
		t.Fatalf("got %d envelopes, want 1 (same tenant.stack batches across modifiers)", len(envs))
	}
	rcpts := gjson.Get(envs[0], "_txc.lmtp.rcpt").Array()
	if len(rcpts) != 2 {
		t.Errorf("rcpt sublist len = %d, want 2", len(rcpts))
	}
	// Both modifiers visible in rcpt[i] — the source of truth.
	gotRcpts := []string{rcpts[0].String(), rcpts[1].String()}
	wantA := "acme.support+monday@chassis.example"
	wantB := "acme.support+tuesday@chassis.example"
	if !(gotRcpts[0] == wantA && gotRcpts[1] == wantB) &&
		!(gotRcpts[0] == wantB && gotRcpts[1] == wantA) {
		t.Errorf("rcpt sublist = %v, want both %q and %q", gotRcpts, wantA, wantB)
	}
}
