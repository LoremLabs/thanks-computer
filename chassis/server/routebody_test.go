package server

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
	"github.com/tidwall/gjson"
)

// TestRouteBodyTenantGate locks the tenant boundary at txco://route: the tenant
// is established once (the _sys→concrete boot handoff) and may only be
// re-affirmed — a tenant op cannot SWITCH tenants via
// `SET @route.tenant=other; EXEC txco://route`. The stack re-pin / goto still
// work, so cross-stack dispatch is unaffected.
func TestRouteBodyTenantGate(t *testing.T) {
	tenant := func(out string) gjson.Result { return gjson.Get(out, "_txc.tenant") }

	// establish (the REAL ingress/boot case): dispatchEnvelope stamps
	// `_txc.tenant=_sys` before the boot pipeline, so the current value at
	// boot/100 is "_sys", NOT empty. route must promote the proposed tenant —
	// this is the one-way _sys→concrete handoff maybeRetenant relies on. A
	// regression that allowed only cur=="" silently dropped this, so the jump
	// into the tenant stack never happened and every routed request fell back to
	// the empty `{}` projection.
	out := routeBody([]byte(`{"_txc":{"tenant":"` + tenants.SystemTenantSlug + `","route":{"to":"x/0","tenant":"acme"}}}`))
	if gjson.Get(out, "_txc.goto").String() != "x/0" {
		t.Fatalf("goto not promoted: %s", out)
	}
	if tenant(out).String() != "acme" {
		t.Fatalf("establish from _sys: tenant should be promoted to acme; %s", out)
	}

	// establish from empty (defensive — a non-boot caller with no tenant set).
	out = routeBody([]byte(`{"_txc":{"route":{"to":"x/0","tenant":"acme"}}}`))
	if tenant(out).String() != "acme" {
		t.Fatalf("establish from empty: tenant should be set to acme; %s", out)
	}

	// re-affirm: route.tenant == current → keeps it.
	out = routeBody([]byte(`{"_txc":{"tenant":"acme","route":{"to":"x/0","tenant":"acme"}}}`))
	if tenant(out).String() != "acme" {
		t.Fatalf("re-affirm: tenant should stay acme; %s", out)
	}

	// SWITCH attempt: current=acme, route.tenant=victim → route must NOT set the
	// tenant, so the per-scope merge keeps the established acme. The goto + a stack
	// re-pin still promote (cross-stack dispatch keeps working).
	out = routeBody([]byte(`{"_txc":{"tenant":"acme","route":{"to":"pub/0","stack":"pub","tenant":"victim"}}}`))
	if tenant(out).Exists() {
		t.Fatalf("cross-tenant switch must be ignored (output must not set _txc.tenant); %s", out)
	}
	if gjson.Get(out, "_txc.goto").String() != "pub/0" {
		t.Fatalf("goto should still promote on a switch attempt; %s", out)
	}
	if gjson.Get(out, "_txc.stack").String() != "pub" {
		t.Fatalf("stack re-pin should still work on a switch attempt; %s", out)
	}
}
