package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// batchSettings drives handleBatchStackSettings (POST /stacks/settings, no
// {name} var) and returns the recorder so callers can assert non-200s.
func batchSettings(t *testing.T, c *Controller, req batchStackSettingsRequest) *httptest.ResponseRecorder {
	t.Helper()
	body := mustJSON(t, req)
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/settings", body, nil), testTenant)
	c.handleBatchStackSettings(w, r)
	return w
}

// deployMintingStack creates + activates a one-op stack so it mints a routing
// host (the suffix is configured by the caller's controller).
func deployMintingStack(t *testing.T, c *Controller, name string) {
	t.Helper()
	v := callCreateDraft(t, c, name, "")
	callPutFiles(t, c, name, v, []stackFile{{Path: "100/main.txcl", Content: `EXEC "http://x/y"`}})
	callActivate(t, c, name, v)
	if n, _ := systemHostRow(t, c, name); n != 1 {
		t.Fatalf("setup %s: want 1 minted host, got %d", name, n)
	}
}

// TestBatchStackSettingsMatchHeadlessRevokes: --match flips mint_hostname on
// EVERY matching stack and (with --force) revokes their live URLs in one call,
// while leaving non-matching stacks completely untouched. Without --force a
// batch that would revoke live URLs is a 409 that changes nothing.
func TestBatchStackSettingsMatchHeadlessRevokes(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".localhost",
	})
	// Two stacks match "publications"; "shop" does not.
	deployMintingStack(t, c, "publications/a")
	deployMintingStack(t, c, "publications/b")
	deployMintingStack(t, c, "shop")

	mintFalse := false

	// --match publications WITHOUT force → 409; nothing changed.
	if w := batchSettings(t, c, batchStackSettingsRequest{Match: "publications", MintHostname: &mintFalse}); w.Code != http.StatusConflict {
		t.Fatalf("batch no-host w/o force: got %d, want 409 (body=%s)", w.Code, w.Body.String())
	}
	if m := stackMintCol(t, c, "publications/a"); m != 1 {
		t.Fatalf("after 409: publications/a mint=%d, want 1 (unchanged)", m)
	}
	if n, _ := systemHostRow(t, c, "publications/a"); n != 1 {
		t.Fatalf("after 409: publications/a host count=%d, want 1 (not revoked)", n)
	}

	// --match publications WITH force → 200; both flipped + revoked.
	w := batchSettings(t, c, batchStackSettingsRequest{Match: "publications", MintHostname: &mintFalse, Force: true})
	if w.Code != http.StatusOK {
		t.Fatalf("batch no-host w/ force: got %d (body=%s)", w.Code, w.Body.String())
	}
	var resp batchStackSettingsResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	if resp.Matched != 2 {
		t.Fatalf("matched=%d, want 2", resp.Matched)
	}
	if resp.MintHostname {
		t.Fatal("response mint_hostname=true, want false")
	}
	if len(resp.RevokedHosts) != 2 {
		t.Fatalf("revoked %d hosts, want 2 (%v)", len(resp.RevokedHosts), resp.RevokedHosts)
	}
	for _, s := range []string{"publications/a", "publications/b"} {
		if m := stackMintCol(t, c, s); m != 0 {
			t.Fatalf("%s: mint=%d after force, want 0", s, m)
		}
		if n, _ := systemHostRow(t, c, s); n != 0 {
			t.Fatalf("%s: host count=%d after force, want 0 (revoked)", s, n)
		}
	}

	// The non-matching stack is completely untouched.
	if m := stackMintCol(t, c, "shop"); m != 1 {
		t.Fatalf("shop mint=%d, want 1 (must not be touched by --match publications)", m)
	}
	if n, _ := systemHostRow(t, c, "shop"); n != 1 {
		t.Fatalf("shop host count=%d, want 1 (untouched)", n)
	}
}

// TestBatchStackSettingsEmptyMatchRejected: an empty match is a 400, never a
// "hit every stack" footgun.
func TestBatchStackSettingsEmptyMatchRejected(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	mintFalse := false
	if w := batchSettings(t, c, batchStackSettingsRequest{Match: "", MintHostname: &mintFalse}); w.Code != http.StatusBadRequest {
		t.Fatalf("empty match: got %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
}

// TestBatchStackSettingsNoMatch: a match that hits nothing is a 404, distinct
// from a 200 that silently changed zero stacks.
func TestBatchStackSettingsNoMatch(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	mintTrue := true
	if w := batchSettings(t, c, batchStackSettingsRequest{Match: "nope", MintHostname: &mintTrue}); w.Code != http.StatusNotFound {
		t.Fatalf("no match: got %d, want 404 (body=%s)", w.Code, w.Body.String())
	}
}
