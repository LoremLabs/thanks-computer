package server

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestRouteBodyTenantGate locks the tenant boundary at txco://route: the tenant
// is established once (boot, when none is set) and may only be re-affirmed — a
// tenant op cannot SWITCH tenants via `SET @route.tenant=other; EXEC txco://route`.
// The stack re-pin / goto still work, so cross-stack dispatch is unaffected.
func TestRouteBodyTenantGate(t *testing.T) {
	tenant := func(out string) gjson.Result { return gjson.Get(out, "_txc.tenant") }

	// establish: no current tenant → route sets it (the ingress/boot case).
	out := routeBody([]byte(`{"_txc":{"route":{"to":"x/0","tenant":"acme"}}}`))
	if gjson.Get(out, "_txc.goto").String() != "x/0" {
		t.Fatalf("goto not promoted: %s", out)
	}
	if tenant(out).String() != "acme" {
		t.Fatalf("establish: tenant should be set to acme; %s", out)
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
