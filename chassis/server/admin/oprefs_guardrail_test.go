package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

func callValidate(t *testing.T, c *Controller, stack string, n int64) (int, validateResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodGet,
		"/v1/tenants/default/stacks/"+stack+"/versions/"+strconv.FormatInt(n, 10)+"/validate", nil,
		map[string]string{"name": stack, "n": strconv.FormatInt(n, 10)}), testTenant)
	c.handleValidateVersion(w, r)
	var resp validateResponse
	if w.Code == http.StatusOK {
		decodeJSON(t, w.Body.Bytes(), &resp)
	}
	return w.Code, resp
}

func opsCount(t *testing.T, c *Controller, stack string) int {
	t.Helper()
	var n int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ops WHERE tenant_id = ? AND stack = ?`,
		testTenant, stack).Scan(&n); err != nil {
		t.Fatalf("count ops: %v", err)
	}
	return n
}

// Activate must reject a draft whose rule still contains an unresolved
// op:// (the admin-UI path bypasses CLI oprefs substitution). The
// activation transaction rolls back — no ops rows materialised.
func TestActivateRejectsUnresolvedOpRef(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	v := callCreateDraft(t, c, "asyncstack", "")
	callPutFiles(t, c, "asyncstack", v, []stackFile{
		{Path: "300/ask-wren.txcl", Content: `EXEC "op://RESEARCH" WITH mode = "async"`},
	})

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/asyncstack/activate",
		mustJSON(t, activateRequest{VersionNumber: v}),
		map[string]string{"name": "asyncstack"}), testTenant)
	c.handleActivateStack(w, r)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("activate code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "unresolved_op_ref") || !strings.Contains(body, "RESEARCH") {
		t.Fatalf("error body missing code/name: %s", body)
	}
	if got := opsCount(t, c, "asyncstack"); got != 0 {
		t.Fatalf("ops rows after rejected activate = %d, want 0 (rollback)", got)
	}
}

// A resolved http(s):// rule still activates (regression guard).
func TestActivateAllowsResolvedURL(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	v := callCreateDraft(t, c, "okstack", "")
	callPutFiles(t, c, "okstack", v, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://localhost:9009/research" WITH mode = "async"`},
	})
	resp := callActivate(t, c, "okstack", v) // asserts 200 internally
	if resp.VersionNumber != v {
		t.Fatalf("activate = %+v, want v%d", resp, v)
	}
	if got := opsCount(t, c, "okstack"); got != 1 {
		t.Fatalf("ops rows = %d, want 1", got)
	}
}

// op:// only inside a `#` comment is NOT a ref (oprefs.HasRefs excludes
// unquoted/commented occurrences) — must still activate.
func TestActivateOpRefInCommentAllowed(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	v := callCreateDraft(t, c, "cmtstack", "")
	callPutFiles(t, c, "cmtstack", v, []stackFile{
		{Path: "100/main.txcl", Content: "# example: op://FOO is just docs\nEXEC \"http://x/y\""},
	})
	if resp := callActivate(t, c, "cmtstack", v); resp.VersionNumber != v {
		t.Fatalf("commented op:// should activate, got %+v", resp)
	}
}

// Validate surfaces the unresolved op:// as a per-file error before the
// author tries to activate.
func TestValidateFlagsUnresolvedOpRef(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	v := callCreateDraft(t, c, "valstack", "")
	callPutFiles(t, c, "valstack", v, []stackFile{
		{Path: "100/good.txcl", Content: `EXEC "http://x/y"`},
		{Path: "300/bad.txcl", Content: `EXEC "op://RESEARCH"`},
	})

	code, resp := callValidate(t, c, "valstack", v)
	if code != http.StatusOK {
		t.Fatalf("validate HTTP code = %d, want 200", code)
	}
	if resp.OK {
		t.Fatalf("validate OK = true, want false (unresolved op://)")
	}
	if resp.Checked != 2 {
		t.Fatalf("checked = %d, want 2", resp.Checked)
	}
	var found bool
	for _, e := range resp.Errors {
		if e.Path == "300/bad.txcl" && strings.Contains(e.Err, "op://RESEARCH") {
			found = true
		}
		if e.Path == "100/good.txcl" {
			t.Fatalf("resolved rule wrongly flagged: %+v", e)
		}
	}
	if !found {
		t.Fatalf("expected op://RESEARCH error for 300/bad.txcl, got %+v", resp.Errors)
	}
}
