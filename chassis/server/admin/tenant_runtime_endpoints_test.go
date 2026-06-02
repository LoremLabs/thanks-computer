package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// The hardcoded test schema (testschemas_test.go) predates the 0014
// migration, so create the table the handlers write.
const tenantRuntimeStateDDL = `CREATE TABLE IF NOT EXISTS tenant_runtime_state (
    tenant_id   TEXT PRIMARY KEY,
    enabled     INTEGER NOT NULL DEFAULT 1,
    suspended   INTEGER NOT NULL DEFAULT 0,
    deny_status INTEGER NOT NULL DEFAULT 403,
    deny_reason TEXT    NOT NULL DEFAULT '',
    rate_limit_rps    INTEGER NOT NULL DEFAULT 0,
    rate_burst        INTEGER NOT NULL DEFAULT 0,
    concurrency_limit INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT    NOT NULL DEFAULT ''
);`

func newRuntimeStateTestController(t *testing.T) *Controller {
	t.Helper()
	c := newTestController(t, config.Config{Personalities: "admin", AuthMode: "open"})
	if _, err := c.pu.RuntimeDB.Exec(tenantRuntimeStateDDL); err != nil {
		t.Fatalf("create tenant_runtime_state: %v", err)
	}
	return c
}

// withSuperAdminTenantContext stamps the resolved-tenant super-admin context
// that resolveTenantMiddleware would produce for an open/super-admin caller.
func withSuperAdminTenantContext(req *http.Request, tenantID, slug string) *http.Request {
	ac := &auth.Context{
		Source:     "open",
		ActorID:    "actor_test",
		SuperAdmin: true,
		TenantID:   tenantID,
		TenantSlug: slug,
	}
	return req.WithContext(auth.WithContext(req.Context(), ac))
}

func runtimeStateRow(t *testing.T, c *Controller, tenantID string) (suspended, denyStatus int, denyReason string, ok bool) {
	t.Helper()
	row := c.pu.RuntimeDB.QueryRow(
		`SELECT suspended, deny_status, deny_reason FROM tenant_runtime_state WHERE tenant_id=?`, tenantID)
	if err := row.Scan(&suspended, &denyStatus, &denyReason); err != nil {
		return 0, 0, "", false
	}
	return suspended, denyStatus, denyReason, true
}

func TestSuspendThenResumeTenant(t *testing.T) {
	c := newRuntimeStateTestController(t) // tnt_default seeded by runtimeSchemaSQL

	body := mustJSON(t, map[string]any{"deny_status": 402, "deny_reason": "payment_required"})
	req := withSuperAdminTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/suspend", bytes.NewReader(body)), "tnt_default", "default")
	rr := httptest.NewRecorder()
	c.handleSuspendTenant(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("suspend status=%d body=%s", rr.Code, rr.Body.String())
	}
	var rec tenantRuntimeStateRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !rec.Suspended || rec.DenyStatus != 402 || rec.DenyReason != "payment_required" {
		t.Errorf("unexpected record: %+v", rec)
	}
	if s, ds, dr, ok := runtimeStateRow(t, c, "tnt_default"); !ok || s != 1 || ds != 402 || dr != "payment_required" {
		t.Errorf("row after suspend: suspended=%d deny_status=%d reason=%q ok=%v", s, ds, dr, ok)
	}

	req = withSuperAdminTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/resume", bytes.NewReader([]byte(`{}`))), "tnt_default", "default")
	rr = httptest.NewRecorder()
	c.handleResumeTenant(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("resume status=%d body=%s", rr.Code, rr.Body.String())
	}
	if s, _, _, ok := runtimeStateRow(t, c, "tnt_default"); !ok || s != 0 {
		t.Errorf("row after resume: suspended=%d ok=%v, want 0", s, ok)
	}
}

func TestSuspendTenantDefaultsTo402(t *testing.T) {
	c := newRuntimeStateTestController(t)
	req := withSuperAdminTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/suspend", bytes.NewReader([]byte(`{}`))), "tnt_default", "default")
	rr := httptest.NewRecorder()
	c.handleSuspendTenant(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if s, ds, dr, ok := runtimeStateRow(t, c, "tnt_default"); !ok || s != 1 || ds != 402 || dr != "payment_required" {
		t.Errorf("row: suspended=%d deny_status=%d reason=%q ok=%v", s, ds, dr, ok)
	}
}

func TestSuspendTenantRejectsNonSuperAdmin(t *testing.T) {
	c := newRuntimeStateTestController(t)
	ac := &auth.Context{Source: "signed", ActorID: "actor_x", TenantID: "tnt_default", TenantSlug: "default",
		Capabilities: []string{"hostname:*:write"}}
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/suspend", bytes.NewReader([]byte(`{}`)))
	req = req.WithContext(auth.WithContext(req.Context(), ac))
	rr := httptest.NewRecorder()
	c.handleSuspendTenant(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-super-admin suspend status=%d, want 403", rr.Code)
	}
}
