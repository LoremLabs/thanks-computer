package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

func activateExpecting(t *testing.T, c *Controller, stack string, n int64, expected *int64) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/"+stack+"/activate",
		mustJSON(t, activateRequest{VersionNumber: n, ExpectedActive: expected}),
		map[string]string{"name": stack}), testTenant)
	c.handleActivateStack(w, r)
	return w
}

// TestActivateExpectedActiveCAS — the ref compare-and-swap: an activation
// that names the version it believes is active is refused with 409
// stack_moved when the chassis moved; nil skips the check (pointer moves,
// --force); a rollback with the right expectation is allowed.
func TestActivateExpectedActiveCAS(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	i := func(n int64) *int64 { return &n }

	v1 := callCreateDraft(t, c, "cas", "")
	callPutFiles(t, c, "cas", v1, []stackFile{{Path: "100/x.txcl", Content: `EMIT .v = 1`}})
	// Nothing active yet: the client expects 0.
	if w := activateExpecting(t, c, "cas", v1, i(0)); w.Code != http.StatusOK {
		t.Fatalf("first activate (expected 0): %d %s", w.Code, w.Body.String())
	}

	v2 := callCreateDraft(t, c, "cas", "")
	callPutFiles(t, c, "cas", v2, []stackFile{{Path: "100/x.txcl", Content: `EMIT .v = 2`}})
	// A stale client (still thinks nothing / v0 is active) is refused…
	w := activateExpecting(t, c, "cas", v2, i(0))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "stack_moved") {
		t.Fatalf("stale expectation: %d %s", w.Code, w.Body.String())
	}
	if n := opsCount(t, c, "cas"); n != 1 {
		t.Fatalf("refused activate must not touch ops: %d", n)
	}
	// …and the draft is still a draft (the tx rolled back).
	if w := activateExpecting(t, c, "cas", v2, i(v1)); w.Code != http.StatusOK {
		t.Fatalf("correct expectation: %d %s", w.Code, w.Body.String())
	}
	// Rollback to v1 from a client that knows v2 is active: fine.
	if w := activateExpecting(t, c, "cas", v1, i(v2)); w.Code != http.StatusOK {
		t.Fatalf("rollback with correct expectation: %d %s", w.Code, w.Body.String())
	}
	// No expectation = no check (explicit pointer move / --force).
	if w := activateExpecting(t, c, "cas", v2, nil); w.Code != http.StatusOK {
		t.Fatalf("unconditional activate: %d %s", w.Code, w.Body.String())
	}
}
