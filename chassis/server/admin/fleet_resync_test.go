package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// withSuperAdminCtx stamps a signed super_admin context (RequireSuperAdmin
// needs the real flag for signed sources, which withAdminCtx doesn't set).
func withSuperAdminCtx(req *http.Request) *http.Request {
	c := &auth.Context{
		Source:       "signed",
		ActorID:      "actor_test",
		KeyID:        "key_test",
		SuperAdmin:   true,
		Capabilities: []string{"admin:all"},
	}
	return req.WithContext(auth.WithContext(req.Context(), c))
}

func TestFleetResyncReemitsTenantState(t *testing.T) {
	c := newTestController(t, config.Config{FeedSink: "file"})
	withAStore(t, c)
	ctx := context.Background()

	// Seed one tenant + a hostname + an active stack version.
	const tid = "tnt_resync"
	if err := c.tenants.Create(ctx, tenants.Tenant{TenantID: tid, Slug: "resynctest"}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if _, err := c.tenants.CreateHostname(ctx, tenants.Hostname{
		Hostname: "h.stacks.test", TenantID: tid, Stack: "web",
	}); err != nil {
		t.Fatalf("create hostname: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at)
		 VALUES ('stk_r','tnt_resync','web',1,'2026-06-02T00:00:00Z')`,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at)
		 VALUES (1,'stk_r',1,'superseded','test','2026-06-02T00:00:00Z')`,
		`INSERT INTO stack_files (version_id, path, content) VALUES (1,'100/x.txcl','NOOP')`,
		`INSERT INTO cron_settings (tenant_id, timezone, updated_at) VALUES ('tnt_resync','Asia/Tokyo','2026-06-02T00:00:00Z')`,
	} {
		if _, err := c.pu.RuntimeDB.Exec(stmt); err != nil {
			t.Fatalf("seed: %v\n%s", err, stmt)
		}
	}

	// Resync that tenant.
	req := withSuperAdminCtx(muxRequest(http.MethodPost, "/v1/fleet/resync",
		mustJSON(t, fleetResyncRequest{TenantSlug: "resynctest"}), nil))
	w := httptest.NewRecorder()
	c.handleFleetResync(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("resync code = %d body=%s", w.Code, w.Body.String())
	}
	var resp fleetResyncResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.FleetEnabled {
		t.Fatalf("fleet_enabled false; want true")
	}
	if resp.Events.TenantCreated != 1 || resp.Events.HostnameBound != 1 || resp.Events.StackActivated != 1 {
		t.Fatalf("counts = %+v, want 1/1/1", resp.Events)
	}
	if resp.Events.CronSettingsUpserted != 1 {
		t.Fatalf("cron_settings_upserted = %d, want 1", resp.Events.CronSettingsUpserted)
	}

	// One outbox row of each type for the tenant.
	for _, et := range []string{"tenant.created", "hostname.bound", "stack.activated", "cron.settings.upserted"} {
		var n int
		if err := c.pu.RuntimeDB.QueryRow(
			`SELECT count(*) FROM control_events_outbox WHERE event_type = ? AND tenant_id = ?`,
			et, tid).Scan(&n); err != nil {
			t.Fatalf("outbox query: %v", err)
		}
		if n != 1 {
			t.Fatalf("event %s: outbox count = %d, want 1", et, n)
		}
	}
}

func TestFleetResyncRequiresTenant(t *testing.T) {
	c := newTestController(t, config.Config{FeedSink: "file"})
	withAStore(t, c)

	// No slug → 400 (no fleet-wide fan-out).
	req := withSuperAdminCtx(muxRequest(http.MethodPost, "/v1/fleet/resync",
		mustJSON(t, fleetResyncRequest{}), nil))
	w := httptest.NewRecorder()
	c.handleFleetResync(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing slug: code = %d body=%s, want 400", w.Code, w.Body.String())
	}

	// Unknown slug → 404.
	req2 := withSuperAdminCtx(muxRequest(http.MethodPost, "/v1/fleet/resync",
		mustJSON(t, fleetResyncRequest{TenantSlug: "nope"}), nil))
	w2 := httptest.NewRecorder()
	c.handleFleetResync(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("unknown slug: code = %d, want 404", w2.Code)
	}
}

func TestFleetResyncForbiddenWithoutSuperAdmin(t *testing.T) {
	c := newTestController(t, config.Config{FeedSink: "file"})
	withAStore(t, c)
	// withAdminCtx is signed admin:all but NOT super_admin → forbidden.
	req := withAdminCtx(muxRequest(http.MethodPost, "/v1/fleet/resync",
		mustJSON(t, fleetResyncRequest{TenantSlug: "x"}), nil))
	w := httptest.NewRecorder()
	c.handleFleetResync(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-super-admin: code = %d, want 403", w.Code)
	}
}
