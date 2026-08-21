package processor

// Run-level regression for the _sys/boot operator-hook routing pattern
// (broken by the "_txc lockdown"; see systemMayWriteTxc): a path-gated
// boot hook owned by the system tenant proposes a route via
// `EMIT @route.*`, and the proposal must survive the EMIT overlay + merge
// so boot/100's `WHEN @route.to != ""` can promote it. The same rule
// authored by a concrete tenant must keep losing its proposal.

import (
	"context"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// autoRouteHook mirrors the shipped examples (mcp-server boot/75,
// playground boot/25): path-gated, fires only when nothing routed yet.
const autoRouteHook = `WHEN @route.to == ""
  && @src == "http"
  && @web.req.url.path =~ /^\/mcp(\/.*)?$/
  EMIT @route.tenant = "default",
       @route.stack = "mcp-server",
       @route.to = "mcp-server/0"`

func seedTenantAndOp(t *testing.T, pu *Unit, tenantID, slug, stack string, scope int, txcl string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(
		`INSERT OR IGNORE INTO tenants (tenant_id, slug, name, created_at) VALUES (?, ?, ?, '')`,
		tenantID, slug, slug); err != nil {
		t.Fatalf("seed tenant %s: %v", slug, err)
	}
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, '', ?, '', '')`,
		tenantID, stack, scope, txcl); err != nil {
		t.Fatalf("seed op: %v", err)
	}
}

func runBootOnce(t *testing.T, pu *Unit, ctx context.Context, raw, stage string) string {
	t.Helper()
	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(ctx, raw, stage, resCh) }()
	select {
	case p := <-resCh:
		return p.Raw
	case <-time.After(3 * time.Second):
		t.Fatalf("no response within 3s for %s", stage)
		return ""
	}
}

func TestBootHookEmitRouteProposalSurvives(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedTenantAndOp(t, pu, tenants.SystemTenantID, tenants.SystemTenantSlug, "boot", 25, autoRouteHook)

	raw := `{"_txc":{"tenant":"_sys","src":"http","web":{"req":{"url":{"path":"/mcp"}}}}}`
	got := runBootOnce(t, pu, WithTenant(context.Background(), tenants.SystemTenantSlug), raw, "boot/0")

	if g := gjson.Get(got, "_txc.route.to").String(); g != "mcp-server/0" {
		t.Errorf("system boot hook's route proposal lost (lockdown regression): %s", got)
	}
	if g := gjson.Get(got, "_txc.route.stack").String(); g != "mcp-server" {
		t.Errorf("_txc.route.stack = %q, want mcp-server: %s", g, got)
	}
	// The hook proposes; it must NOT have re-tenanted by itself — the pin
	// promotion belongs to txco://route + maybeRetenant.
	if g := gjson.Get(got, "_txc.tenant").String(); g != "_sys" {
		t.Errorf("_txc.tenant = %q, want _sys untouched by the proposal: %s", g, got)
	}
}

func TestTenantHookEmitRouteProposalStillDropped(t *testing.T) {
	pu, _ := newTestUnit(t)
	seedTenantAndOp(t, pu, "tnt_acme", "acme", "app", 25, autoRouteHook)

	raw := `{"_txc":{"tenant":"acme","src":"http","web":{"req":{"url":{"path":"/mcp"}}}}}`
	got := runBootOnce(t, pu, WithTenant(context.Background(), "acme"), raw, "app/0")

	if gjson.Get(got, "_txc.route").Exists() {
		t.Errorf("tenant-authored route proposal survived (guard bypass): %s", got)
	}
}
