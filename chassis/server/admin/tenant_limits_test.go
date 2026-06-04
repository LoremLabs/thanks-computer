package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

func runtimeLimits(t *testing.T, c *Controller, tenantID string) (rps float64, burst, conc, enabled int) {
	t.Helper()
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT rate_limit_rps, rate_burst, concurrency_limit, enabled
		   FROM tenant_runtime_state WHERE tenant_id=?`, tenantID).
		Scan(&rps, &burst, &conc, &enabled); err != nil {
		t.Fatalf("read limits: %v", err)
	}
	return
}

func TestSetTenantLimits(t *testing.T) {
	c := newRuntimeStateTestController(t)
	// rps only — burst defaults to ceil(2*rps); concurrency left at 0.
	body := mustJSON(t, map[string]any{"rps": 5})
	req := withSuperAdminTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/limits", bytes.NewReader(body)), "tnt_default", "default")
	rr := httptest.NewRecorder()
	c.handleSetTenantLimits(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	rps, burst, conc, _ := runtimeLimits(t, c, "tnt_default")
	if rps != 5 || burst != 10 || conc != 0 {
		t.Errorf("limits = rps %v burst %d conc %d, want 5/10/0 (burst=ceil(2*rps))", rps, burst, conc)
	}
}

// TestSuspendPreservesLimits is the forward-compat fix: suspend/resume
// read-modify-write the full row, so they never clobber rate/concurrency.
func TestSuspendPreservesLimits(t *testing.T) {
	c := newRuntimeStateTestController(t)

	post := func(path string, body []byte, h func(http.ResponseWriter, *http.Request)) {
		t.Helper()
		req := withSuperAdminTenantContext(httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)), "tnt_default", "default")
		rr := httptest.NewRecorder()
		h(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}

	post("/v1/tenants/default/limits", mustJSON(t, map[string]any{"rps": 3, "concurrency": 7}), c.handleSetTenantLimits)
	post("/v1/tenants/default/suspend", []byte(`{}`), c.handleSuspendTenant)

	rps, _, conc, enabled := runtimeLimits(t, c, "tnt_default")
	if enabled != 0 {
		t.Errorf("enabled=%d, want 0 (operator disable owns the enabled column)", enabled)
	}
	if rps != 3 || conc != 7 {
		t.Errorf("suspend clobbered limits: rps %v conc %d, want 3/7", rps, conc)
	}

	post("/v1/tenants/default/resume", []byte(`{}`), c.handleResumeTenant)
	rps, _, conc, enabled = runtimeLimits(t, c, "tnt_default")
	if enabled != 1 || rps != 3 || conc != 7 {
		t.Errorf("resume state: enabled=%d rps=%v conc=%d, want 1/3/7", enabled, rps, conc)
	}
}
