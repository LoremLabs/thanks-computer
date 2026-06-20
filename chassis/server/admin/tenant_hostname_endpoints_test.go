package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// seedStackForTest inserts a minimal stacks row so the hostname-create
// handler's `stack_not_found` check passes.
func seedStackForTest(t *testing.T, c *Controller, tenantID, name string) {
	t.Helper()
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at)
		 VALUES (?, ?, ?, '2026-01-01T00:00:00Z')`,
		"stk_"+name, tenantID, name); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
}

func newHostnameTestController(t *testing.T) *Controller {
	t.Helper()
	c := newTestController(t, config.Config{Personalities: "admin", AuthMode: "open"})
	// Default tenant is seeded by runtimeSchemaSQL; also seed a stack
	// so create handler passes the existence check.
	seedStackForTest(t, c, "tnt_default", "default/web")
	return c
}

// withTenantContext stamps a synthetic admin context (admin:all, default
// tenant) onto the request — mirrors what resolveTenantMiddleware
// would do for a successfully-authenticated tenant-scoped request.
func withTenantContext(req *http.Request, tenantID string) *http.Request {
	ac := &auth.Context{
		Source:       "signed",
		ActorID:      "actor_test",
		Capabilities: []string{"admin:all"},
		TenantID:     tenantID,
		TenantSlug:   "default",
	}
	return req.WithContext(auth.WithContext(req.Context(), ac))
}

// TestCreateHostnameHappyPath — POST creates a row, reload happens,
// the response body has the canonical hostname.
func TestCreateHostnameHappyPath(t *testing.T) {
	c := newHostnameTestController(t)
	body := mustJSON(t, map[string]any{
		"hostname": "Foo.Local",
		"stack":    "default/web",
	})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames", bytes.NewReader(body)), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleCreateHostname(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got hostnameRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Hostname != "foo.local" {
		t.Errorf("hostname not canonicalized: %q", got.Hostname)
	}
	if got.Stack != "default/web" || got.TenantID != "tnt_default" {
		t.Errorf("unexpected record: %+v", got)
	}
	if got.CreatedBy != "actor_test" {
		t.Errorf("created_by: got %q, want actor_test", got.CreatedBy)
	}
}

// TestMintHostname — POST /hostnames/mint mints a structured host bound to the
// BASE stack, accepting a mail-only `<stack>/_mail` as proof the stack is real,
// and stamps verified_at (so it's an immediate sender).
func TestMintHostname(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin", AuthMode: "open", StructuredHostSuffix: ".stacks.example"})
	// Mail-only deployment: only the `_mail` channel exists, no web `autoreply`.
	seedStackForTest(t, c, "tnt_default", "autoreply/_mail")

	body := mustJSON(t, map[string]any{"stack": "autoreply"})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames/mint", bytes.NewReader(body)), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleMintHostname(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct{ Hostname, Stack, URL string }
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Stack != "autoreply" {
		t.Errorf("stack = %q, want autoreply", got.Stack)
	}
	if !strings.HasPrefix(got.Hostname, "autoreply-") || !strings.HasSuffix(got.Hostname, ".stacks.example") {
		t.Errorf("hostname = %q, want autoreply-<rand>.stacks.example", got.Hostname)
	}
	// The minted row binds to the base stack and is verified (an immediate sender).
	var stack, verifiedAt string
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT stack, verified_at FROM tenant_hostnames WHERE hostname=?`, got.Hostname,
	).Scan(&stack, &verifiedAt); err != nil {
		t.Fatalf("query minted row: %v", err)
	}
	if stack != "autoreply" || verifiedAt == "" {
		t.Errorf("minted row stack=%q verified_at=%q, want autoreply + non-empty", stack, verifiedAt)
	}
}

// TestMintHostnameStackNotFound — minting for a stack with neither a web nor a
// `_mail` presence is rejected, so we never bind a host that routes to nothing.
func TestMintHostnameStackNotFound(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin", AuthMode: "open", StructuredHostSuffix: ".stacks.example"})
	body := mustJSON(t, map[string]any{"stack": "ghost"})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames/mint", bytes.NewReader(body)), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleMintHostname(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// TestMintHostnameMissingStack — stack is required.
func TestMintHostnameMissingStack(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin", AuthMode: "open", StructuredHostSuffix: ".stacks.example"})
	body := mustJSON(t, map[string]any{})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames/mint", bytes.NewReader(body)), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleMintHostname(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rr.Code)
	}
}

// TestCreateHostnameZoneCoveredAutoVerifies — adding a hostname that falls
// inside a zone THIS tenant has delegated to us is auto-verified (the NS
// delegation is the proof of control); no dns-txt challenge needed.
func TestCreateHostnameZoneCoveredAutoVerifies(t *testing.T) {
	c := newHostnameTestController(t) // seeds tnt_default (slug "default") + stack default/web
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO dns_zones (id, tenant_id, origin, mname, rname, created_at, updated_at, verified_at)
		 VALUES ('dz_test','tnt_default','example.test','ns1.test.','hostmaster.example.test.','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed zone: %v", err)
	}

	// Apex of the delegated zone — should auto-verify (no challenge).
	body := mustJSON(t, map[string]any{"hostname": "example.test", "stack": "default/web"})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames", bytes.NewReader(body)), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleCreateHostname(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got hostnameRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.VerifiedAt == "" {
		t.Errorf("hostname in own delegated zone should be auto-verified, got empty verified_at: %+v", got)
	}

	// A hostname NOT covered by the zone still requires verification.
	body2 := mustJSON(t, map[string]any{"hostname": "elsewhere.test", "stack": "default/web"})
	req2 := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames", bytes.NewReader(body2)), "tnt_default")
	rr2 := httptest.NewRecorder()
	c.handleCreateHostname(rr2, req2)
	var got2 hostnameRecord
	_ = json.Unmarshal(rr2.Body.Bytes(), &got2)
	if got2.VerifiedAt != "" {
		t.Errorf("hostname outside any zone should NOT be auto-verified: %+v", got2)
	}
}

// TestCreateHostnameInvalidHostname — strict admin-write rejection.
func TestCreateHostnameInvalidHostname(t *testing.T) {
	c := newHostnameTestController(t)
	for _, bad := range []string{"", "1.2.3.4", "::1", "host:bad:port", "-leading.com"} {
		body := mustJSON(t, map[string]any{"hostname": bad, "stack": "default/web"})
		req := withTenantContext(httptest.NewRequest(http.MethodPost,
			"/v1/tenants/default/hostnames", bytes.NewReader(body)), "tnt_default")
		rr := httptest.NewRecorder()
		c.handleCreateHostname(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("input=%q: got %d, want 400", bad, rr.Code)
		}
		if !strings.Contains(rr.Body.String(), "invalid_hostname") {
			t.Errorf("input=%q: missing invalid_hostname in body: %s", bad, rr.Body.String())
		}
	}
}

// TestCreateHostnameMissingStack — pointing at a non-existent stack
// in the caller's tenant returns 400 stack_not_found.
func TestCreateHostnameMissingStack(t *testing.T) {
	c := newHostnameTestController(t)
	body := mustJSON(t, map[string]any{
		"hostname": "good.local",
		"stack":    "default/does-not-exist",
	})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames", bytes.NewReader(body)), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleCreateHostname(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "stack_not_found") {
		t.Errorf("missing stack_not_found: %s", rr.Body.String())
	}
}

// TestCreateHostnameConflict — second create for an already-claimed
// hostname returns 409 with the existing owner's slug.
func TestCreateHostnameConflict(t *testing.T) {
	c := newHostnameTestController(t)
	// First claim.
	body := mustJSON(t, map[string]any{"hostname": "taken.local", "stack": "default/web"})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames", bytes.NewReader(body)), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleCreateHostname(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first claim status=%d body=%s", rr.Code, rr.Body.String())
	}
	// Second claim — same body, same tenant.
	req2 := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames", bytes.NewReader(body)), "tnt_default")
	rr2 := httptest.NewRecorder()
	c.handleCreateHostname(rr2, req2)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d body=%s", rr2.Code, rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), "hostname_owned") {
		t.Errorf("missing hostname_owned: %s", rr2.Body.String())
	}
	if !strings.Contains(rr2.Body.String(), `"tenant":"default"`) {
		t.Errorf("missing owner slug: %s", rr2.Body.String())
	}
}

// TestListHostnames — POST + GET round-trip; only active rows by
// default, with ?history=true surfacing revoked rows too.
func TestListHostnames(t *testing.T) {
	c := newHostnameTestController(t)
	body := mustJSON(t, map[string]any{"hostname": "alpha.local", "stack": "default/web"})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames", bytes.NewReader(body)), "tnt_default")
	c.handleCreateHostname(httptest.NewRecorder(), req)

	// Active listing.
	getReq := withTenantContext(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/hostnames", nil), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleListHostnames(rr, getReq)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d", rr.Code)
	}
	var resp listHostnamesResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Hostnames) != 1 || resp.Hostnames[0].Hostname != "alpha.local" {
		t.Errorf("active list: %+v", resp.Hostnames)
	}

	// Revoke, then history mode should still show one row (revoked).
	if _, err := c.pu.RuntimeDB.Exec(
		`UPDATE tenant_hostnames SET revoked_at = '2026-02-01T00:00:00Z' WHERE hostname = 'alpha.local'`); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	getReq2 := withTenantContext(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/hostnames?history=true", nil), "tnt_default")
	rr2 := httptest.NewRecorder()
	c.handleListHostnames(rr2, getReq2)
	var resp2 listHostnamesResponse
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(resp2.Hostnames) != 1 || resp2.Hostnames[0].RevokedAt == "" {
		t.Errorf("history list: %+v", resp2.Hostnames)
	}
	// Without history, the active list is empty.
	getReq3 := withTenantContext(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/hostnames", nil), "tnt_default")
	rr3 := httptest.NewRecorder()
	c.handleListHostnames(rr3, getReq3)
	var resp3 listHostnamesResponse
	if err := json.Unmarshal(rr3.Body.Bytes(), &resp3); err != nil {
		t.Fatalf("decode active: %v", err)
	}
	if len(resp3.Hostnames) != 0 {
		t.Errorf("active list after revoke should be empty, got %+v", resp3.Hostnames)
	}
}

// TestRevokeHostnameHappyPath — DELETE returns 200 with the revoked
// flag set; a second DELETE on the same hostname is idempotent.
func TestRevokeHostnameHappyPath(t *testing.T) {
	c := newHostnameTestController(t)
	body := mustJSON(t, map[string]any{"hostname": "del.local", "stack": "default/web"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/default/hostnames",
			bytes.NewReader(body)), "tnt_default"))

	rr := httptest.NewRecorder()
	req := mustMuxVarsRequest(t, http.MethodDelete,
		"/v1/tenants/default/hostnames/del.local", "tnt_default",
		map[string]string{"hostname": "del.local"})
	c.handleRevokeHostname(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"revoked":true`) {
		t.Errorf("missing revoked flag: %s", rr.Body.String())
	}

	// Idempotent second delete.
	rr2 := httptest.NewRecorder()
	req2 := mustMuxVarsRequest(t, http.MethodDelete,
		"/v1/tenants/default/hostnames/del.local", "tnt_default",
		map[string]string{"hostname": "del.local"})
	c.handleRevokeHostname(rr2, req2)
	if rr2.Code != http.StatusOK {
		t.Errorf("second delete status=%d", rr2.Code)
	}
}

// TestCreateHostnameWithoutStack — the decoupled flow: claim without
// --stack succeeds, the row exists with an empty stack, and the
// response makes that visible to the caller. Routing won't honor
// the row until it's attached.
func TestCreateHostnameWithoutStack(t *testing.T) {
	c := newHostnameTestController(t)
	body := mustJSON(t, map[string]any{"hostname": "unattached.local"})
	req := withTenantContext(httptest.NewRequest(http.MethodPost,
		"/v1/tenants/default/hostnames", bytes.NewReader(body)), "tnt_default")
	rr := httptest.NewRecorder()
	c.handleCreateHostname(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got hostnameRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Stack != "" {
		t.Errorf("Stack: got %q, want empty", got.Stack)
	}
	if got.Hostname != "unattached.local" {
		t.Errorf("Hostname: %q", got.Hostname)
	}
}

// TestAttachHostnameHappyPath — claim unattached, then attach. The
// row's stack is updated and echoed.
func TestAttachHostnameHappyPath(t *testing.T) {
	c := newHostnameTestController(t)
	// Step 1: claim unattached.
	createBody := mustJSON(t, map[string]any{"hostname": "attachme.local"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/default/hostnames",
			bytes.NewReader(createBody)), "tnt_default"))
	// Step 2: attach.
	attachBody := mustJSON(t, map[string]any{"stack": "default/web"})
	rr := httptest.NewRecorder()
	req := mustMuxVarsRequest(t, http.MethodPost,
		"/v1/tenants/default/hostnames/attachme.local/attach", "tnt_default",
		map[string]string{"hostname": "attachme.local"})
	req.Body = io.NopCloser(bytes.NewReader(attachBody))
	req.Header.Set("Content-Type", "application/json")
	c.handleAttachHostname(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("attach status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got hostnameRecord
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Stack != "default/web" {
		t.Errorf("Stack after attach: %q", got.Stack)
	}
}

// TestAttachHostnameStackNotFound — attach to a stack that doesn't
// exist in the tenant returns 400 stack_not_found.
func TestAttachHostnameStackNotFound(t *testing.T) {
	c := newHostnameTestController(t)
	createBody := mustJSON(t, map[string]any{"hostname": "bad-attach.local"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/default/hostnames",
			bytes.NewReader(createBody)), "tnt_default"))
	attachBody := mustJSON(t, map[string]any{"stack": "default/missing"})
	rr := httptest.NewRecorder()
	req := mustMuxVarsRequest(t, http.MethodPost,
		"/v1/tenants/default/hostnames/bad-attach.local/attach", "tnt_default",
		map[string]string{"hostname": "bad-attach.local"})
	req.Body = io.NopCloser(bytes.NewReader(attachBody))
	req.Header.Set("Content-Type", "application/json")
	c.handleAttachHostname(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "stack_not_found") {
		t.Errorf("missing stack_not_found: %s", rr.Body.String())
	}
}

// TestAttachHostnameCrossTenant — tenant A claims a hostname; tenant
// B can't attach it to tenant B's stack. Returns 404 (don't leak
// existence) — same shape as the cross-tenant guard on verify.
func TestAttachHostnameCrossTenant(t *testing.T) {
	c := newHostnameTestController(t)
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_other', 'other', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}
	seedStackForTest(t, c, "tnt_other", "other/web")
	// Tenant 'other' claims the hostname unattached.
	createBody := mustJSON(t, map[string]any{"hostname": "shared.local"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/other/hostnames",
			bytes.NewReader(createBody)), "tnt_other"))

	// Tenant 'default' attempts to attach it to one of its own stacks.
	attachBody := mustJSON(t, map[string]any{"stack": "default/web"})
	rr := httptest.NewRecorder()
	req := mustMuxVarsRequest(t, http.MethodPost,
		"/v1/tenants/default/hostnames/shared.local/attach", "tnt_default",
		map[string]string{"hostname": "shared.local"})
	req.Body = io.NopCloser(bytes.NewReader(attachBody))
	req.Header.Set("Content-Type", "application/json")
	c.handleAttachHostname(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-tenant attach: got %d, want 404 (don't leak existence)", rr.Code)
	}
}

// TestChallengeAndVerifyCrossTenant — caller in tenant A can't
// challenge or verify a hostname row owned by tenant B via URL
// manipulation. The handler reads the row's tenant_id and compares
// to ac.TenantID (not just trusting the URL slug). Returns 404 (not
// 403, not 200) per the http-status-discipline memory: don't leak
// existence to cross-tenant probes.
func TestChallengeAndVerifyCrossTenant(t *testing.T) {
	c := newHostnameTestController(t)
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_other', 'other', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}
	seedStackForTest(t, c, "tnt_other", "other/web")
	body := mustJSON(t, map[string]any{"hostname": "other.local", "stack": "other/web"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/other/hostnames",
			bytes.NewReader(body)), "tnt_other"))

	// Tenant default attempts to challenge other.local.
	challengeBody := mustJSON(t, map[string]any{"method": "dns-txt"})
	rr := httptest.NewRecorder()
	req := mustMuxVarsRequest(t, http.MethodPost,
		"/v1/tenants/default/hostnames/other.local/challenges", "tnt_default",
		map[string]string{"hostname": "other.local"})
	req.Body = io.NopCloser(bytes.NewReader(challengeBody))
	req.Header.Set("Content-Type", "application/json")
	c.handleCreateChallenge(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-tenant challenge: got %d, want 404 (don't leak existence)", rr.Code)
	}

	// Same for /verify.
	rr2 := httptest.NewRecorder()
	req2 := mustMuxVarsRequest(t, http.MethodPost,
		"/v1/tenants/default/hostnames/other.local/verify", "tnt_default",
		map[string]string{"hostname": "other.local"})
	c.handleVerifyHostname(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Errorf("cross-tenant verify: got %d, want 404", rr2.Code)
	}
}

// TestRevokeHostnameCrossTenant — caller in tenant A can't release a
// hostname owned by tenant B. Returns 403.
func TestRevokeHostnameCrossTenant(t *testing.T) {
	c := newHostnameTestController(t)
	// Seed a second tenant + a stack so we can claim a hostname there.
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_other', 'other', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed other tenant: %v", err)
	}
	seedStackForTest(t, c, "tnt_other", "other/web")
	body := mustJSON(t, map[string]any{"hostname": "other.local", "stack": "other/web"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/other/hostnames",
			bytes.NewReader(body)), "tnt_other"))

	// Caller in tnt_default attempts to revoke other.local.
	rr := httptest.NewRecorder()
	req := mustMuxVarsRequest(t, http.MethodDelete,
		"/v1/tenants/default/hostnames/other.local", "tnt_default",
		map[string]string{"hostname": "other.local"})
	c.handleRevokeHostname(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("cross-tenant revoke: got %d, want 403", rr.Code)
	}
}

// TestCreateChallengeIdempotent — calling POST /challenges twice for
// the same (hostname, method) without force=true returns the SAME
// token on the second call (status 200, reused=true). This is the
// internal docs/todo-custom-domains.md §6a fix: re-running `add` or `challenge`
// after the operator has pasted a token into DNS must not invalidate
// that DNS record.
func TestCreateChallengeIdempotent(t *testing.T) {
	c := newHostnameTestController(t)
	body := mustJSON(t, map[string]any{"hostname": "idempotent.local", "stack": "default/web"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/default/hostnames",
			bytes.NewReader(body)), "tnt_default"))

	mint := func(reqBody []byte) (*httptest.ResponseRecorder, challengeRecord) {
		rr := httptest.NewRecorder()
		req := mustMuxVarsRequest(t, http.MethodPost,
			"/v1/tenants/default/hostnames/idempotent.local/challenges", "tnt_default",
			map[string]string{"hostname": "idempotent.local"})
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		c.handleCreateChallenge(rr, req)
		var got challengeRecord
		if rr.Code == http.StatusCreated || rr.Code == http.StatusOK {
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v body=%s", err, rr.Body.String())
			}
		}
		return rr, got
	}

	// First call: fresh mint → 201, reused=false, rotated=false.
	rr1, first := mint(mustJSON(t, map[string]any{"method": "dns-txt"}))
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first call: status=%d body=%s", rr1.Code, rr1.Body.String())
	}
	if first.Token == "" {
		t.Fatal("first call: empty token")
	}
	if first.Reused || first.Rotated {
		t.Errorf("first call: reused=%v rotated=%v; both should be false", first.Reused, first.Rotated)
	}

	// Second call without force: SAME token, status 200, reused=true.
	rr2, second := mint(mustJSON(t, map[string]any{"method": "dns-txt"}))
	if rr2.Code != http.StatusOK {
		t.Fatalf("second call: status=%d (want 200 reused), body=%s", rr2.Code, rr2.Body.String())
	}
	if second.Token != first.Token {
		t.Errorf("second call: token rotated unexpectedly\n  first=%q\n second=%q", first.Token, second.Token)
	}
	if !second.Reused {
		t.Error("second call: reused=false; should be true on idempotent path")
	}
	if second.Rotated {
		t.Error("second call: rotated=true; should be false on idempotent path")
	}

	// Third call WITH force=true: rotates → new token, status 201,
	// rotated=true, reused=false. This is the operator's opt-in to the
	// old behaviour for the "I really want a new token" case.
	rr3, third := mint(mustJSON(t, map[string]any{"method": "dns-txt", "force": true}))
	if rr3.Code != http.StatusCreated {
		t.Fatalf("force call: status=%d (want 201 rotated), body=%s", rr3.Code, rr3.Body.String())
	}
	if third.Token == first.Token {
		t.Error("force call: token did not change after --rotate")
	}
	if !third.Rotated {
		t.Error("force call: rotated=false; should be true after revoke+mint")
	}
	if third.Reused {
		t.Error("force call: reused=true; mutually exclusive with rotated")
	}
}

// TestHostnameStatusReadsWithoutRotating — GET /status returns the
// active challenge token AND a subsequent (non-force) POST /challenges
// reuses it. Proves the read endpoint doesn't mutate state — the whole
// point of adding it (internal docs/todo-custom-domains.md §6a).
func TestHostnameStatusReadsWithoutRotating(t *testing.T) {
	c := newHostnameTestController(t)
	body := mustJSON(t, map[string]any{"hostname": "statusread.local", "stack": "default/web"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/default/hostnames",
			bytes.NewReader(body)), "tnt_default"))

	// Issue a challenge to put a token in flight.
	rrCh := httptest.NewRecorder()
	reqCh := mustMuxVarsRequest(t, http.MethodPost,
		"/v1/tenants/default/hostnames/statusread.local/challenges", "tnt_default",
		map[string]string{"hostname": "statusread.local"})
	reqCh.Body = io.NopCloser(bytes.NewReader(mustJSON(t, map[string]any{"method": "dns-txt"})))
	reqCh.Header.Set("Content-Type", "application/json")
	c.handleCreateChallenge(rrCh, reqCh)
	if rrCh.Code != http.StatusCreated {
		t.Fatalf("seed challenge: status=%d body=%s", rrCh.Code, rrCh.Body.String())
	}
	var seeded challengeRecord
	_ = json.Unmarshal(rrCh.Body.Bytes(), &seeded)

	// GET /status returns the seeded token.
	rrSt := httptest.NewRecorder()
	reqSt := mustMuxVarsRequest(t, http.MethodGet,
		"/v1/tenants/default/hostnames/statusread.local/status", "tnt_default",
		map[string]string{"hostname": "statusread.local"})
	c.handleHostnameStatus(rrSt, reqSt)
	if rrSt.Code != http.StatusOK {
		t.Fatalf("status: code=%d body=%s", rrSt.Code, rrSt.Body.String())
	}
	var st hostnameStatusResponse
	if err := json.Unmarshal(rrSt.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.Hostname != "statusread.local" {
		t.Errorf("status hostname: got %q, want statusread.local", st.Hostname)
	}
	if len(st.ActiveChallenges) != 1 {
		t.Fatalf("status active_challenges: got %d, want 1; body=%s", len(st.ActiveChallenges), rrSt.Body.String())
	}
	if st.ActiveChallenges[0].Token != seeded.Token {
		t.Errorf("status token: got %q, want seeded %q", st.ActiveChallenges[0].Token, seeded.Token)
	}
	if st.ActiveChallenges[0].Expired {
		t.Error("status: fresh challenge reported expired=true")
	}

	// Re-issue (no force) AFTER the GET: must reuse the SAME token —
	// confirming the GET didn't mutate the row.
	rrRe := httptest.NewRecorder()
	reqRe := mustMuxVarsRequest(t, http.MethodPost,
		"/v1/tenants/default/hostnames/statusread.local/challenges", "tnt_default",
		map[string]string{"hostname": "statusread.local"})
	reqRe.Body = io.NopCloser(bytes.NewReader(mustJSON(t, map[string]any{"method": "dns-txt"})))
	reqRe.Header.Set("Content-Type", "application/json")
	c.handleCreateChallenge(rrRe, reqRe)
	if rrRe.Code != http.StatusOK {
		t.Fatalf("re-issue after status: code=%d body=%s (status mutated?)", rrRe.Code, rrRe.Body.String())
	}
	var reused challengeRecord
	_ = json.Unmarshal(rrRe.Body.Bytes(), &reused)
	if reused.Token != seeded.Token {
		t.Errorf("status mutated state: token changed seeded=%q reused=%q", seeded.Token, reused.Token)
	}
	if !reused.Reused {
		t.Error("re-issue: reused=false after status read")
	}
}

// TestHostnameStatusNoActiveChallenge — a freshly-claimed hostname
// with no challenge yet returns a 200 with the binding and an empty
// active_challenges slice. Lets the CLI render "no active challenge"
// without distinguishing 404-from-no-row vs 200-with-empty.
func TestHostnameStatusNoActiveChallenge(t *testing.T) {
	c := newHostnameTestController(t)
	body := mustJSON(t, map[string]any{"hostname": "bare.local", "stack": "default/web"})
	c.handleCreateHostname(httptest.NewRecorder(), withTenantContext(
		httptest.NewRequest(http.MethodPost, "/v1/tenants/default/hostnames",
			bytes.NewReader(body)), "tnt_default"))

	rr := httptest.NewRecorder()
	req := mustMuxVarsRequest(t, http.MethodGet,
		"/v1/tenants/default/hostnames/bare.local/status", "tnt_default",
		map[string]string{"hostname": "bare.local"})
	c.handleHostnameStatus(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status code=%d body=%s", rr.Code, rr.Body.String())
	}
	var st hostnameStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(st.ActiveChallenges) != 0 {
		t.Errorf("active_challenges: got %d, want 0", len(st.ActiveChallenges))
	}
	if st.Hostname != "bare.local" {
		t.Errorf("hostname: got %q", st.Hostname)
	}
}

// mustMuxVarsRequest builds a request with synthetic gorilla/mux vars
// stamped on it via mux.SetURLVars (since handlers read `hostname`
// out of vars rather than re-parsing the URL).
func mustMuxVarsRequest(t *testing.T, method, url, tenantID string, vars map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, url, nil)
	req = setURLVarsForTest(req, vars)
	return withTenantContext(req, tenantID)
}

// setURLVarsForTest is a thin shim over gorilla/mux's SetURLVars — kept
// out-of-line so the import isn't tested-file-noise.
func setURLVarsForTest(r *http.Request, vars map[string]string) *http.Request {
	return setURLVarsImpl(r, vars)
}

// _ = context import is used implicitly via httptest.NewRequest.
var _ = context.Background
