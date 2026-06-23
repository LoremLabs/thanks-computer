package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// patchSettings drives handlePatchStackSettings and returns the raw recorder,
// so callers can assert non-200 statuses (e.g. the 409 force gate).
func patchSettings(t *testing.T, c *Controller, stack string, req patchStackSettingsRequest) *httptest.ResponseRecorder {
	t.Helper()
	body := mustJSON(t, req)
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPatch,
		"/v1/tenants/default/stacks/"+stack+"/settings", body,
		map[string]string{"name": stack}), testTenant)
	c.handlePatchStackSettings(w, r)
	return w
}

// callPatchSettings drives handlePatchStackSettings, asserts 200, and returns
// the decoded response. (No force; for the force path use patchSettings.)
func callPatchSettings(t *testing.T, c *Controller, stack string, mint bool) stackSettingsResponse {
	t.Helper()
	w := patchSettings(t, c, stack, patchStackSettingsRequest{MintHostname: &mint})
	if w.Code != http.StatusOK {
		t.Fatalf("patch settings %s: got %d body=%s", stack, w.Code, w.Body.String())
	}
	var resp stackSettingsResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	return resp
}

func stackMintCol(t *testing.T, c *Controller, stack string) int {
	t.Helper()
	var mint int
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT mint_hostname FROM stacks WHERE tenant_id=? AND name=?`,
		testTenant, stack).Scan(&mint); err != nil {
		t.Fatalf("read mint_hostname: %v", err)
	}
	return mint
}

// TestNoHostOnLiveURLRequiresForce: once a stack has minted a URL, --no-host
// without --force is a 409 that changes nothing; --force revokes the live host,
// returns it in RevokedHosts, and flips the column.
func TestNoHostOnLiveURLRequiresForce(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
	})
	// Deploy a default stack → it mints a routing host.
	v := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v, []stackFile{{Path: "100/main.txcl", Content: `EXEC "http://x/y"`}})
	callActivate(t, c, "shop", v)
	if n, _ := systemHostRow(t, c, "shop"); n != 1 {
		t.Fatalf("setup: want 1 host, got %d", n)
	}

	// --no-host WITHOUT force → 409; nothing changes.
	mintFalse := false
	if w := patchSettings(t, c, "shop", patchStackSettingsRequest{MintHostname: &mintFalse}); w.Code != http.StatusConflict {
		t.Fatalf("no-host w/o force: got %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	if n, _ := systemHostRow(t, c, "shop"); n != 1 {
		t.Fatalf("after 409: host count=%d, want 1 (must not revoke)", n)
	}
	if m := stackMintCol(t, c, "shop"); m != 1 {
		t.Fatalf("after 409: mint_hostname=%d, want 1 (must not flip)", m)
	}

	// --no-host WITH force → 200, host revoked, listed in response, column flipped.
	w := patchSettings(t, c, "shop", patchStackSettingsRequest{MintHostname: &mintFalse, Force: true})
	if w.Code != http.StatusOK {
		t.Fatalf("no-host w/ force: got %d (body=%s)", w.Code, w.Body.String())
	}
	var resp stackSettingsResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	if resp.MintHostname {
		t.Fatal("after force: mint_hostname=true, want false")
	}
	if len(resp.RevokedHosts) != 1 {
		t.Fatalf("after force: revoked %d hosts, want 1 (%v)", len(resp.RevokedHosts), resp.RevokedHosts)
	}
	if n, _ := systemHostRow(t, c, "shop"); n != 0 {
		t.Fatalf("after force: live host count=%d, want 0 (revoked)", n)
	}
	if m := stackMintCol(t, c, "shop"); m != 0 {
		t.Fatalf("after force: mint_hostname=%d, want 0", m)
	}
}

// TestNoHostNoLiveURLNeedsNoForce: --no-host on a stack with no live URL just
// flips the column — no force required, nothing to revoke.
func TestNoHostNoLiveURLNeedsNoForce(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
	})
	resp := callPatchSettings(t, c, "never-deployed", false)
	if resp.MintHostname || len(resp.RevokedHosts) != 0 {
		t.Fatalf("got mint=%v revoked=%v, want false/none", resp.MintHostname, resp.RevokedHosts)
	}
}

func callGetStack(t *testing.T, c *Controller, stack string) stackRecord {
	t.Helper()
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodGet,
		"/v1/tenants/default/stacks/"+stack, nil,
		map[string]string{"name": stack}), testTenant)
	c.handleGetStack(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("get stack %s: got %d body=%s", stack, w.Code, w.Body.String())
	}
	var rec stackRecord
	decodeJSON(t, w.Body.Bytes(), &rec)
	return rec
}

// TestPatchStackSettingsVivifiesAndFlips: PATCH --no-host on a stack that has
// never been applied vivifies the stacks row and persists mint_hostname=0; GET
// surfaces it; --host re-enables. This is what lets a developer opt out BEFORE
// first apply, so the URL never mints.
func TestPatchStackSettingsVivifiesAndFlips(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	// Stack does not exist yet.
	resp := callPatchSettings(t, c, "biography", false)
	if resp.MintHostname {
		t.Fatalf("after --no-host: mint_hostname=true, want false")
	}
	// Row was vivified with the flag persisted.
	var mint int
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT mint_hostname FROM stacks WHERE tenant_id=? AND name=?`,
		testTenant, "biography").Scan(&mint); err != nil {
		t.Fatalf("stacks row not vivified: %v", err)
	}
	if mint != 0 {
		t.Fatalf("mint_hostname=%d, want 0", mint)
	}
	// GET projects the column.
	if got := callGetStack(t, c, "biography"); got.MintHostname {
		t.Fatalf("GET MintHostname=true, want false")
	}
	// Re-enable.
	if resp2 := callPatchSettings(t, c, "biography", true); !resp2.MintHostname {
		t.Fatalf("after --host: mint_hostname=false, want true")
	}
	if got := callGetStack(t, c, "biography"); !got.MintHostname {
		t.Fatalf("GET MintHostname=false after --host, want true")
	}
}

// TestPatchStackSettingsRequiresField: an empty body is a 400, not a silent
// no-op that vivifies an empty stack.
func TestPatchStackSettingsRequiresField(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPatch,
		"/v1/tenants/default/stacks/x/settings", []byte(`{}`),
		map[string]string{"name": "x"}), testTenant)
	c.handlePatchStackSettings(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty settings: got %d, want 400", w.Code)
	}
	// And it must NOT have vivified a row.
	var n int
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT count(*) FROM stacks WHERE tenant_id=? AND name=?`, testTenant, "x").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("empty PATCH vivified %d stacks rows, want 0", n)
	}
}

// TestHeadlessStackMintsNoHostname: with the structured-host suffix configured
// (so a normal stack WOULD mint), a stack toggled headless before activate
// mints zero routing hosts. The companion TestActivateMintsStructuredHostname
// proves the default still mints, so this isolates the gate.
func TestHeadlessStackMintsNoHostname(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
	})
	callPatchSettings(t, c, "shop", false) // opt out before first apply

	v := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://x/y"`},
	})
	callActivate(t, c, "shop", v)

	if n, host := systemHostRow(t, c, "shop"); n != 0 {
		t.Fatalf("headless stack minted %d host rows (%q), want 0", n, host)
	}
}

// TestReenableHostMintsAgain: the lifecycle — headless suppresses the mint,
// then `--host` + re-activate mints one. (Suppress-future-mint: the gate is
// read fresh on each activate.)
func TestReenableHostMintsAgain(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
	})
	callPatchSettings(t, c, "shop", false)
	v := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v, []stackFile{{Path: "100/main.txcl", Content: `EXEC "http://x/y"`}})
	callActivate(t, c, "shop", v)
	if n, _ := systemHostRow(t, c, "shop"); n != 0 {
		t.Fatalf("headless: want 0 host rows, got %d", n)
	}

	callPatchSettings(t, c, "shop", true) // re-enable
	v2 := callCreateDraft(t, c, "shop", "")
	callPutFiles(t, c, "shop", v2, []stackFile{{Path: "100/main.txcl", Content: `EXEC "http://x/z"`}})
	callActivate(t, c, "shop", v2)
	if n, _ := systemHostRow(t, c, "shop"); n != 1 {
		t.Fatalf("after re-enable: want 1 host row, got %d", n)
	}
}
