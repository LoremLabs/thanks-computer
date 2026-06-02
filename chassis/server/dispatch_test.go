package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// stubResolver lets tests dictate exactly what Resolve returns,
// without spinning up the full bus loop or YAML loader.
type stubResolver struct {
	target ingress.RouteTarget
	hit    bool
}

func (s *stubResolver) Resolve(ingress.RouteKey) (ingress.RouteTarget, bool) {
	return s.target, s.hit
}

// TestDispatchEnvelopeStampsSystemTenant: every request now enters the
// editable _sys/boot pipeline. dispatchEnvelope no longer resolves the
// tenant (that moved to txco://detect-tenant); it just stamps the
// reserved system tenant and dispatches to the concrete boot/0 stage,
// pinned to _sys so the boot ops resolve tenant-scoped.
func TestDispatchEnvelopeStampsSystemTenant(t *testing.T) {
	in := `{"_txc":{"src":"http","web":{"req":{"host":"anywhere.local"}}}}`
	out, stage := dispatchEnvelope(in, "fallthrough")
	if stage != defaultEntryStage {
		t.Errorf("stage = %q, want %q", stage, defaultEntryStage)
	}
	if stage != "boot/0" {
		t.Errorf("defaultEntryStage must be the concrete boot/0 (sparse pipeline), got %q", stage)
	}
	if got := gjson.Get(out, "_txc.tenant").String(); got != "_sys" {
		t.Errorf("_txc.tenant = %q, want %q", got, "_sys")
	}
	if got := gjson.Get(out, "_txc.stack").String(); got != "boot" {
		t.Errorf("_txc.stack = %q, want %q", got, "boot")
	}
	if got := gjson.Get(out, "_txc.hostname_verified").Bool(); got {
		t.Errorf("_txc.hostname_verified = true, want false (detect-tenant sets it authoritatively)")
	}
	if got := gjson.Get(out, "_txc.src").String(); got != "http" {
		t.Errorf("_txc.src preserved? got %q, want %q", got, "http")
	}
}

// TestDispatchEnvelopeReject — --ingress-miss-action=reject is the
// hard pre-processor 404 toggle: stageNoRoute, envelope untouched, no
// boot pipeline. The bus loop short-circuits to emitNoRouteResponse.
func TestDispatchEnvelopeReject(t *testing.T) {
	in := `{"_txc":{"src":"http","web":{"req":{"host":"unrouted.local"}}}}`
	out, stage := dispatchEnvelope(in, "reject")
	if stage != stageNoRoute {
		t.Errorf("stage = %q, want %q", stage, stageNoRoute)
	}
	if out != in {
		t.Errorf("envelope mutated on reject; got %q, want unchanged", out)
	}
	if got := gjson.Get(out, "_txc.tenant"); got.Exists() {
		t.Errorf("_txc.tenant should not be stamped on reject: %q", got.String())
	}
}

// TestDispatchEnvelopeRejectNonHTTP — reject mode is an HTTP-ingress
// concept; a non-HTTP source (e.g. the cron system tick) must NOT take the
// bare web-404 fast-path. It falls through into the boot pipeline instead
// (where the scope-1000 notfound is itself HTTP-gated, so it halts quietly).
func TestDispatchEnvelopeRejectNonHTTP(t *testing.T) {
	in := `{"_txc":{"src":"cron","cron":{"job":"default"}}}`
	_, stage := dispatchEnvelope(in, "reject")
	if stage == stageNoRoute {
		t.Fatalf("cron tick took the reject 404 fast-path; want fall-through to the boot pipeline")
	}
	if stage != defaultEntryStage {
		t.Errorf("stage = %q, want %q (boot pipeline)", stage, defaultEntryStage)
	}
}

// TestDetectTenantBodyHit: scope-0 DECIDE writes an INERT proposal
// under _txc.route.* and must NOT set _txc.goto/_txc.tenant (no jump,
// no re-tenant yet — scopes 1..99 get to see/modify it first).
func TestDetectTenantBodyHit(t *testing.T) {
	resolver := &stubResolver{
		target: ingress.RouteTarget{Tenant: "t1", Stack: "t1/web", Ingress: "host:t1.local", Verified: true},
		hit:    true,
	}
	body := detectTenantBody(resolver, []byte(`{"_txc":{"src":"http","web":{"req":{"host":"t1.local"}}}}`))
	if got := gjson.Get(body, "_txc.route.tenant").String(); got != "t1" {
		t.Errorf("_txc.route.tenant = %q, want t1", got)
	}
	if got := gjson.Get(body, "_txc.route.stack").String(); got != "t1/web" {
		t.Errorf("_txc.route.stack = %q, want t1/web", got)
	}
	if got := gjson.Get(body, "_txc.route.to").String(); got != "t1/web/0" {
		t.Errorf("_txc.route.to = %q, want t1/web/0", got)
	}
	if got := gjson.Get(body, "_txc.route.ingress").String(); got != "host:t1.local" {
		t.Errorf("_txc.route.ingress = %q, want host:t1.local", got)
	}
	if !gjson.Get(body, "_txc.route.hostname_verified").Bool() {
		t.Errorf("_txc.route.hostname_verified = false, want true")
	}
	// Inert: no active control keys yet.
	if gjson.Get(body, "_txc.goto").Exists() {
		t.Errorf("detect must not set _txc.goto (decide-only); got %q", gjson.Get(body, "_txc.goto").String())
	}
	if gjson.Get(body, "_txc.tenant").Exists() {
		t.Errorf("detect must not set _txc.tenant (decide-only); got %q", gjson.Get(body, "_txc.tenant").String())
	}
}

// TestDetectTenantBodyContinuation: a continuation poll proposes a
// jump to the internal _sys txc-continuation stack with NO tenant.
func TestDetectTenantBodyContinuation(t *testing.T) {
	resolver := &stubResolver{hit: false}
	body := detectTenantBody(resolver, []byte(`{"_txc":{"continuation":"rcid_abc"}}`))
	if got := gjson.Get(body, "_txc.route.to").String(); got != "txc-continuation/0" {
		t.Errorf("_txc.route.to = %q, want txc-continuation/0", got)
	}
	if gjson.Get(body, "_txc.route.tenant").Exists() {
		t.Errorf("continuation route must carry no tenant; got %q", gjson.Get(body, "_txc.route.tenant").String())
	}
	if gjson.Get(body, "_txc.goto").Exists() {
		t.Errorf("detect must not set _txc.goto (decide-only)")
	}
}

// TestDetectTenantBodyCron: a per-tenant cron tick (src=cron with a
// trusted _txc.cron.tenant stamped by the controller) proposes a route
// into that tenant's _cron/0, even though the resolver would miss.
func TestDetectTenantBodyCron(t *testing.T) {
	resolver := &stubResolver{hit: false}
	body := detectTenantBody(resolver, []byte(`{"_txc":{"src":"cron","cron":{"tenant":"acme","job":"_cron"}}}`))
	if got := gjson.Get(body, "_txc.route.to").String(); got != "_cron/0" {
		t.Errorf("_txc.route.to = %q, want _cron/0", got)
	}
	if got := gjson.Get(body, "_txc.route.tenant").String(); got != "acme" {
		t.Errorf("_txc.route.tenant = %q, want acme", got)
	}
	if got := gjson.Get(body, "_txc.route.stack").String(); got != "_cron" {
		t.Errorf("_txc.route.stack = %q, want _cron", got)
	}
	if gjson.Get(body, "_txc.tenant").Exists() || gjson.Get(body, "_txc.goto").Exists() {
		t.Errorf("detect must stay decide-only (no _txc.tenant/_txc.goto)")
	}
}

// A bare cron tick with no _txc.cron.tenant is the legacy system-wide
// "default" job: it must NOT take the per-tenant branch and instead
// fall through to the resolver (here a miss → "{}").
func TestDetectTenantBodyCronLegacyUnchanged(t *testing.T) {
	resolver := &stubResolver{hit: false}
	if body := detectTenantBody(resolver, []byte(`{"_txc":{"src":"cron","cron":{"job":"default"}}}`)); body != "{}" {
		t.Errorf("legacy cron body = %q, want {} (resolver miss)", body)
	}
}

// TestDetectTenantBodyMiss: on a resolver miss the transform returns
// `{}` (no proposal), so the gated route op is skipped and _sys/boot
// scope 1000 serves the 404.
func TestDetectTenantBodyMiss(t *testing.T) {
	resolver := &stubResolver{hit: false}
	if body := detectTenantBody(resolver, []byte(`{"_txc":{"src":"http","web":{"req":{"host":"nope.local"}}}}`)); body != "{}" {
		t.Errorf("miss body = %q, want {}", body)
	}
}

// TestRouteBody: scope-100 EXECUTE promotes the inert _txc.route.*
// proposal into the active control keys.
func TestRouteBody(t *testing.T) {
	// Hostname hit proposal → goto + tenant + parity keys.
	hit := `{"_txc":{"route":{"tenant":"t1","stack":"t1/web","to":"t1/web/0","ingress":"host:t1.local","hostname_verified":true}}}`
	b := routeBody([]byte(hit))
	if got := gjson.Get(b, "_txc.goto").String(); got != "t1/web/0" {
		t.Errorf("_txc.goto = %q, want t1/web/0", got)
	}
	if got := gjson.Get(b, "_txc.tenant").String(); got != "t1" {
		t.Errorf("_txc.tenant = %q, want t1", got)
	}
	if got := gjson.Get(b, "_txc.stack").String(); got != "t1/web" {
		t.Errorf("_txc.stack = %q, want t1/web", got)
	}
	if got := gjson.Get(b, "_txc.ingress").String(); got != "host:t1.local" {
		t.Errorf("_txc.ingress = %q, want host:t1.local", got)
	}
	if !gjson.Get(b, "_txc.hostname_verified").Bool() {
		t.Errorf("_txc.hostname_verified = false, want true")
	}

	// Continuation proposal (no tenant) → goto only, pin stays _sys.
	cont := routeBody([]byte(`{"_txc":{"route":{"to":"txc-continuation/0"}}}`))
	if got := gjson.Get(cont, "_txc.goto").String(); got != "txc-continuation/0" {
		t.Errorf("continuation _txc.goto = %q, want txc-continuation/0", got)
	}
	if gjson.Get(cont, "_txc.tenant").Exists() {
		t.Errorf("continuation must not set _txc.tenant (pin stays _sys); got %q", gjson.Get(cont, "_txc.tenant").String())
	}

	// Defensive: no proposal → {} (the WHEN gate normally prevents this).
	if got := routeBody([]byte(`{"_txc":{"src":"http"}}`)); got != "{}" {
		t.Errorf("no-route body = %q, want {}", got)
	}
}

// TestEmitNoRouteResponse — the synthetic envelope sent on ResCh
// carries the response-envelope shape the web inlet's
// response.go::checkStatus/getOutput pair reads. Body is the stock
// "404 not found\n" message — diagnostic context (which Host got
// rejected) goes to the chassis log via the caller in the bus loop,
// not into the response body the client sees.
func TestEmitNoRouteResponse(t *testing.T) {
	resCh := make(chan event.Payload, 1)
	envelope := &event.Envelope{
		Ctx: context.Background(),
		Payload: &event.Payload{
			Raw:  `{"_txc":{"src":"http","web":{"req":{"host":"unknown.local"}}}}`,
			Type: event.JSON,
		},
		ResCh: resCh,
	}
	emitNoRouteResponse(envelope)
	select {
	case out := <-resCh:
		raw := out.Raw
		if got := gjson.Get(raw, "_txc.web.res.status").Int(); got != 404 {
			t.Errorf("_txc.web.res.status = %d, want 404", got)
		}
		if got := gjson.Get(raw, "_txc.web.res.headers.content-type.0").String(); got != "text/plain; charset=utf-8" {
			t.Errorf("_txc.web.res.headers.content-type.0 = %q, want text/plain; charset=utf-8", got)
		}
		b64 := gjson.Get(raw, "_txc.web.res.body").String()
		if b64 == "" {
			t.Fatal("body missing")
		}
		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("body not base64: %v", err)
		}
		if string(decoded) != "404 not found\n" {
			t.Errorf("body = %q, want \"404 not found\\n\"", string(decoded))
		}
		// The Host header MUST NOT leak into the client-facing body —
		// internal routing config is operator-side only.
		if strings.Contains(string(decoded), "unknown.local") {
			t.Errorf("body leaks Host header: %q", string(decoded))
		}
	default:
		t.Fatal("emitNoRouteResponse did not send on ResCh")
	}
}
