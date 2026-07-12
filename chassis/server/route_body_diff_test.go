package server

import (
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// Frozen copies of the pre-jsonx detectTenantBody / routeBody sjson
// chains. The converted functions must match byte-for-byte across
// every branch.

func detectTenantBodyFrozen(resolver ingress.Resolver, in []byte) string {
	if gjson.GetBytes(in, "_txc.route.to").String() != "" {
		return `{}`
	}
	if gjson.GetBytes(in, "_txc.continuation").String() != "" {
		raw := "{}"
		raw, _ = sjson.Set(raw, "_txc.route.to", "txc-continuation/0")
		return raw
	}
	for _, br := range []struct{ src, tenantPath, stack string }{
		{"cron", "_txc.cron.tenant", "_cron"},
		{"room", "_txc.room.tenant", "_room"},
		{"inspect", "_txc.inspect.tenant", "_inspect"},
		{"scheduled", "_txc.scheduled.tenant", "_scheduled"},
	} {
		if gjson.GetBytes(in, "_txc.src").String() == br.src {
			if ct := gjson.GetBytes(in, br.tenantPath).String(); ct != "" {
				raw := "{}"
				raw, _ = sjson.Set(raw, "_txc.route.tenant", ct)
				raw, _ = sjson.Set(raw, "_txc.route.stack", br.stack)
				raw, _ = sjson.Set(raw, "_txc.route.ingress", br.src)
				raw, _ = sjson.Set(raw, "_txc.route.hostname_verified", true)
				raw, _ = sjson.Set(raw, "_txc.route.to", br.stack+"/0")
				return raw
			}
		}
	}
	target, hit := resolver.Resolve(ingress.KeyFromEnvelope(string(in)))
	if !hit {
		return "{}"
	}
	raw := "{}"
	raw, _ = sjson.Set(raw, "_txc.route.tenant", target.Tenant)
	raw, _ = sjson.Set(raw, "_txc.route.stack", target.Stack)
	raw, _ = sjson.Set(raw, "_txc.route.ingress", target.Ingress)
	raw, _ = sjson.Set(raw, "_txc.route.hostname_verified", target.Verified)
	raw, _ = sjson.Set(raw, "_txc.route.to", target.Stack+"/0")
	return raw
}

func routeBodyFrozen(in []byte) string {
	to := gjson.GetBytes(in, "_txc.route.to").String()
	if to == "" {
		return "{}"
	}
	raw := "{}"
	raw, _ = sjson.Set(raw, "_txc.goto", to)
	if tn := gjson.GetBytes(in, "_txc.route.tenant"); tn.Exists() && tn.String() != "" {
		if cur := gjson.GetBytes(in, "_txc.tenant").String(); cur == "" || cur == tenants.SystemTenantSlug || cur == tn.String() {
			raw, _ = sjson.Set(raw, "_txc.tenant", tn.String())
		}
	}
	if s := gjson.GetBytes(in, "_txc.route.stack"); s.Exists() {
		raw, _ = sjson.Set(raw, "_txc.stack", s.String())
	}
	if ig := gjson.GetBytes(in, "_txc.route.ingress"); ig.Exists() {
		raw, _ = sjson.Set(raw, "_txc.ingress", ig.String())
	}
	if hv := gjson.GetBytes(in, "_txc.route.hostname_verified"); hv.Exists() {
		raw, _ = sjson.Set(raw, "_txc.hostname_verified", hv.Bool())
	}
	return raw
}

var routeDiffEnvelopes = []string{
	`{}`,
	`{"_txc":{"route":{"to":"www/0"}}}`,
	`{"_txc":{"continuation":"CcxRcid"}}`,
	`{"_txc":{"src":"cron","cron":{"tenant":"driplit"}}}`,
	`{"_txc":{"src":"cron"}}`,
	`{"_txc":{"src":"room","room":{"tenant":"t2"}}}`,
	`{"_txc":{"src":"inspect","inspect":{"tenant":"t3"}}}`,
	`{"_txc":{"src":"scheduled","scheduled":{"tenant":"t4"}}}`,
	`{"_txc":{"src":"web","web":{"req":{"host":"www.dripl.it"}}}}`,
	`{"_txc":{"route":{"to":"_cron/0","tenant":"driplit","stack":"_cron","ingress":"cron","hostname_verified":true}}}`,
	`{"_txc":{"route":{"to":"www/0","tenant":"driplit"},"tenant":"_sys"}}`,
	`{"_txc":{"route":{"to":"www/0","tenant":"driplit"},"tenant":"other"}}`,
	`{"_txc":{"route":{"to":"www/0","tenant":"driplit"},"tenant":"driplit"}}`,
	`{"_txc":{"route":{"to":"txc-continuation/0"}}}`,
	`{"_txc":{"route":{"to":"www/0","stack":"www","ingress":"http","hostname_verified":false}}}`,
}

func TestDetectTenantBodyMatchesFrozen(t *testing.T) {
	resolvers := []ingress.Resolver{
		&stubResolver{},
		&stubResolver{hit: true, target: ingress.RouteTarget{Tenant: "driplit", Stack: "www", Ingress: "http", Verified: true}},
		&stubResolver{hit: true, target: ingress.RouteTarget{Stack: "pub", Verified: false}},
	}
	for _, in := range routeDiffEnvelopes {
		for _, r := range resolvers {
			want := detectTenantBodyFrozen(r, []byte(in))
			got := detectTenantBody(r, []byte(in))
			if got != want {
				t.Fatalf("detectTenantBody mismatch for %s\nwant %q\ngot  %q", in, want, got)
			}
		}
	}
}

func TestRouteBodyMatchesFrozen(t *testing.T) {
	for _, in := range routeDiffEnvelopes {
		want := routeBodyFrozen([]byte(in))
		got := routeBody([]byte(in))
		if got != want {
			t.Fatalf("routeBody mismatch for %s\nwant %q\ngot  %q", in, want, got)
		}
	}
}
