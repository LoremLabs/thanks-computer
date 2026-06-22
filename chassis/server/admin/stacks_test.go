package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

const testTenant = "tnt_default"

// muxRequest builds an httptest request whose mux.Vars contain the
// given key/value pairs. The stack handlers read both `name` and `n`
// from the matched route; tests bypass the router and inject the
// vars directly so the handler logic can be exercised without
// wiring the full subrouter chain.
func muxRequest(method, target string, body []byte, vars map[string]string) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	return mux.SetURLVars(r, vars)
}

// decodeJSON unmarshals a recorder body into v or fails the test.
func decodeJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode: %v body=%s", err, string(body))
	}
}

// callCreateDraft is a tiny driver used by several tests to push a
// stack through CreateDraft → PutDraftFiles → Activate. Returns the
// version_number of the activated version.
func callCreateDraft(t *testing.T, c *Controller, stack, from string) int64 {
	t.Helper()
	body := mustJSON(t, createDraftRequest{From: from})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/"+stack+"/draft", body,
		map[string]string{"name": stack}), testTenant)
	c.handleCreateDraft(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("create draft %s: got %d body=%s", stack, w.Code, w.Body.String())
	}
	var resp createDraftResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	return resp.VersionNumber
}

func callPutFiles(t *testing.T, c *Controller, stack string, n int64, files []stackFile) {
	t.Helper()
	body := mustJSON(t, putFilesRequest{Files: files})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPut,
		"/v1/tenants/default/stacks/"+stack+"/versions/"+strconv.FormatInt(n, 10)+"/files", body,
		map[string]string{"name": stack, "n": strconv.FormatInt(n, 10)}), testTenant)
	c.handlePutDraftFiles(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("put files %s v%d: got %d body=%s", stack, n, w.Code, w.Body.String())
	}
}

func callActivate(t *testing.T, c *Controller, stack string, n int64) activateResponse {
	t.Helper()
	body := mustJSON(t, activateRequest{VersionNumber: n})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/"+stack+"/activate", body,
		map[string]string{"name": stack}), testTenant)
	c.handleActivateStack(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("activate %s v%d: got %d body=%s", stack, n, w.Code, w.Body.String())
	}
	var resp activateResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	return resp
}

// TestDeactivateViaEmptyVersion proves the mechanism client.DeactivateStack
// relies on: activating an EMPTY version (no files) clears the stack's
// materialised ops, so it stops serving — while the stack row + version
// history remain (re-deployable). This is the fleet-safe "stop serving"
// that `txco deactivate` performs.
func TestDeactivateViaEmptyVersion(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	v1 := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", v1, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/v1"`},
	})
	callActivate(t, c, "hello", v1)
	if n := helloOpsCount(t, c); n != 1 {
		t.Fatalf("after activate v1: ops=%d, want 1", n)
	}

	// Deactivate = activate an empty version: empty draft + zero files.
	v2 := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", v2, nil)
	callActivate(t, c, "hello", v2)
	if n := helloOpsCount(t, c); n != 0 {
		t.Fatalf("after deactivate (empty v2): ops=%d, want 0 (stack should serve nothing)", n)
	}
}

func helloOpsCount(t *testing.T, c *Controller) int {
	t.Helper()
	var n int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ops WHERE tenant_id = ? AND stack = ?`,
		testTenant, "hello").Scan(&n); err != nil {
		t.Fatalf("count ops: %v", err)
	}
	return n
}

// TestStacksRoundTrip pushes two drafts to one stack, activates each
// in turn, and verifies that ops materialisation, version listing,
// and rollback all behave as documented.
func TestStacksRoundTrip(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	v1 := callCreateDraft(t, c, "hello", "")
	if v1 != 1 {
		t.Fatalf("first draft version_number = %d, want 1", v1)
	}
	callPutFiles(t, c, "hello", v1, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/v1"`},
	})
	resp := callActivate(t, c, "hello", v1)
	if resp.VersionNumber != 1 || resp.PriorVersionNumber != nil {
		t.Errorf("first activate = %+v, want v1 with no prior", resp)
	}

	// ops should now have exactly one row for hello.
	var opsCount int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ops WHERE tenant_id = ? AND stack = ?`,
		testTenant, "hello").Scan(&opsCount); err != nil {
		t.Fatalf("count ops: %v", err)
	}
	if opsCount != 1 {
		t.Errorf("ops count after v1 activate = %d, want 1", opsCount)
	}

	// Push v2 starting from active.
	v2 := callCreateDraft(t, c, "hello", "active")
	if v2 != 2 {
		t.Fatalf("second draft version_number = %d, want 2", v2)
	}
	callPutFiles(t, c, "hello", v2, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/v2"`},
		{Path: "200/audit.txcl", Content: `EXEC "http://example.com/audit"`},
	})
	resp = callActivate(t, c, "hello", v2)
	if resp.VersionNumber != 2 || resp.PriorVersionNumber == nil || *resp.PriorVersionNumber != 1 {
		t.Errorf("second activate = %+v, want v2 with prior=v1", resp)
	}

	// ops should now have two rows for hello — clean replace.
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ops WHERE tenant_id = ? AND stack = ?`,
		testTenant, "hello").Scan(&opsCount); err != nil {
		t.Fatalf("count ops: %v", err)
	}
	if opsCount != 2 {
		t.Errorf("ops count after v2 activate = %d, want 2", opsCount)
	}

	// Roll back: activate v1 again.
	resp = callActivate(t, c, "hello", 1)
	if resp.VersionNumber != 1 || resp.PriorVersionNumber == nil || *resp.PriorVersionNumber != 2 {
		t.Errorf("rollback = %+v, want v1 with prior=v2", resp)
	}
	// ops should be back to one row.
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ops WHERE tenant_id = ? AND stack = ?`,
		testTenant, "hello").Scan(&opsCount); err != nil {
		t.Fatalf("count ops: %v", err)
	}
	if opsCount != 1 {
		t.Errorf("ops count after rollback = %d, want 1", opsCount)
	}

	// versions endpoint should list both versions in reverse order with
	// is_active set on v1.
	w := httptest.NewRecorder()
	c.handleListVersions(w, withTenantAdminCtx(muxRequest(http.MethodGet,
		"/v1/tenants/default/stacks/hello/versions", nil,
		map[string]string{"name": "hello"}), testTenant))
	if w.Code != http.StatusOK {
		t.Fatalf("list versions: %d %s", w.Code, w.Body.String())
	}
	var list listVersionsResponse
	decodeJSON(t, w.Body.Bytes(), &list)
	if len(list.Versions) != 2 {
		t.Fatalf("got %d versions, want 2", len(list.Versions))
	}
	if list.Versions[0].VersionNumber != 2 || list.Versions[1].VersionNumber != 1 {
		t.Errorf("got order %d,%d; want 2,1 (reverse chronological)",
			list.Versions[0].VersionNumber, list.Versions[1].VersionNumber)
	}
	if list.Versions[1].IsActive != true || list.Versions[0].IsActive != false {
		t.Errorf("is_active flags wrong: v2=%v v1=%v (after rollback, want v1=true v2=false)",
			list.Versions[0].IsActive, list.Versions[1].IsActive)
	}
}

// TestPerStackVersionNumbering verifies independent counters across
// stacks: pushing 3 drafts to one stack and 1 to another gives 1,2,3
// and 1, not a shared 1..4 sequence.
func TestPerStackVersionNumbering(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	for i := 0; i < 3; i++ {
		v := callCreateDraft(t, c, "hello", "")
		if v != int64(i+1) {
			t.Errorf("hello draft %d returned version_number %d, want %d", i, v, i+1)
		}
	}
	v := callCreateDraft(t, c, "world", "")
	if v != 1 {
		t.Errorf("world's first draft version_number = %d, want 1 (independent counter)", v)
	}
	// And a second world draft is 2, regardless of hello's counter.
	v = callCreateDraft(t, c, "world", "")
	if v != 2 {
		t.Errorf("world's second draft version_number = %d, want 2", v)
	}
}

// TestPutFilesRejectsNonDraft verifies the conflict path: once a
// version has been activated (status='superseded'), its file set is
// frozen — a subsequent PUT returns 409 with version_not_draft.
func TestPutFilesRejectsNonDraft(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/x"`},
	})
	callActivate(t, c, "hello", n)

	// Now try to PUT to the same (now-superseded) version.
	body := mustJSON(t, putFilesRequest{Files: []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/sneaky"`},
	}})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPut,
		"/v1/tenants/default/stacks/hello/versions/1/files", body,
		map[string]string{"name": "hello", "n": "1"}), testTenant)
	c.handlePutDraftFiles(w, r)
	if w.Code != http.StatusConflict {
		t.Errorf("PUT to non-draft version: got %d, want 409 body=%s", w.Code, w.Body.String())
	}
}

// TestValidateVersion exercises the validate endpoint with one good
// file and one syntactically broken one. The endpoint should return
// 200 with ok=false and one error.
func TestValidateVersion(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/ok.txcl", Content: `EXEC "http://example.com/ok"`},
		{Path: "200/bad.txcl", Content: `EXEC`},
		// Mock files should be skipped by validate even when malformed.
		{Path: "100/mock-request.json", Content: `not-real-json-but-not-checked`},
	})

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/hello/versions/1/validate", nil,
		map[string]string{"name": "hello", "n": "1"}), testTenant)
	c.handleValidateVersion(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("validate: %d %s", w.Code, w.Body.String())
	}
	var resp validateResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	if resp.OK {
		t.Errorf("got ok=true, want false (one broken txcl)")
	}
	if resp.Checked != 2 {
		t.Errorf("got checked=%d, want 2 (mock-request.json should not count)", resp.Checked)
	}
	if len(resp.Errors) != 1 || resp.Errors[0].Path != "200/bad.txcl" {
		t.Errorf("got errors=%+v, want one entry for 200/bad.txcl", resp.Errors)
	}

	// A purely clean version returns ok=true with checked=1.
	n2 := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n2, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/x"`},
	})
	w2 := httptest.NewRecorder()
	c.handleValidateVersion(w2, withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/hello/versions/2/validate", nil,
		map[string]string{"name": "hello", "n": "2"}), testTenant))
	var clean validateResponse
	decodeJSON(t, w2.Body.Bytes(), &clean)
	if !clean.OK || clean.Checked != 1 || len(clean.Errors) != 0 {
		t.Errorf("clean validate = %+v, want ok=true checked=1 errors=[]", clean)
	}
}

// TestDiffVersions verifies the four diff cases: added, changed,
// removed, and the equal-manifest short-circuit.
func TestDiffVersions(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	v1 := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", v1, []stackFile{
		{Path: "100/keep.txcl", Content: `EXEC "http://example.com/keep"`},
		{Path: "100/will-remove.txcl", Content: `EXEC "http://example.com/old"`},
		{Path: "200/will-change.txcl", Content: `EXEC "http://example.com/v1"`},
	})
	callActivate(t, c, "hello", v1)

	v2 := callCreateDraft(t, c, "hello", "active")
	callPutFiles(t, c, "hello", v2, []stackFile{
		{Path: "100/keep.txcl", Content: `EXEC "http://example.com/keep"`},
		{Path: "200/will-change.txcl", Content: `EXEC "http://example.com/v2"`},
		{Path: "300/added.txcl", Content: `EXEC "http://example.com/new"`},
	})

	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodGet,
		"/v1/tenants/default/stacks/hello/diff?v1=1&v2=2", nil,
		map[string]string{"name": "hello"}), testTenant)
	// Recreate request with the query string (muxRequest swallows it inside the path).
	r.URL.RawQuery = "v1=1&v2=2"
	c.handleDiffVersions(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("diff: %d %s", w.Code, w.Body.String())
	}
	var resp diffResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	if resp.Equal {
		t.Errorf("got equal=true for non-trivial diff")
	}
	gotByPath := map[string]diffEntry{}
	for _, e := range resp.Entries {
		gotByPath[e.Path] = e
	}
	if e, ok := gotByPath["100/will-remove.txcl"]; !ok || e.Change != "removed" {
		t.Errorf("100/will-remove.txcl = %+v, want removed", e)
	}
	if e, ok := gotByPath["200/will-change.txcl"]; !ok || e.Change != "changed" {
		t.Errorf("200/will-change.txcl = %+v, want changed", e)
	}
	if e, ok := gotByPath["300/added.txcl"]; !ok || e.Change != "added" {
		t.Errorf("300/added.txcl = %+v, want added", e)
	}
	if _, ok := gotByPath["100/keep.txcl"]; ok {
		t.Errorf("100/keep.txcl appeared in diff but is unchanged: %+v", gotByPath["100/keep.txcl"])
	}

	// Diffing a version against itself returns equal=true with no entries.
	w2 := httptest.NewRecorder()
	r2 := withTenantAdminCtx(muxRequest(http.MethodGet,
		"/v1/tenants/default/stacks/hello/diff?v1=2&v2=2", nil,
		map[string]string{"name": "hello"}), testTenant)
	r2.URL.RawQuery = "v1=2&v2=2"
	c.handleDiffVersions(w2, r2)
	var equal diffResponse
	decodeJSON(t, w2.Body.Bytes(), &equal)
	if !equal.Equal || len(equal.Entries) != 0 {
		t.Errorf("self-diff = %+v, want equal=true entries=[]", equal)
	}
}

// TestActivateConcurrent serialises two parallel activates of the
// same stack — neither should see a half-applied state in ops, and
// the surviving active_version must point to a real version. The
// dbcache enforces MaxOpenConns=1 in production; the in-memory
// sqlite under test does too by default, so this exercises the
// transaction discipline rather than connection pooling.
func TestActivateConcurrent(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	// Three drafts, distinct file sets — concurrent activates pick
	// from these.
	for i := 1; i <= 3; i++ {
		v := callCreateDraft(t, c, "hello", "")
		callPutFiles(t, c, "hello", v, []stackFile{
			{Path: "100/main.txcl", Content: fmt.Sprintf(`EXEC "http://example.com/v%d"`, i)},
		})
	}

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := int64(1); i <= 3; i++ {
		wg.Add(1)
		go func(n int64) {
			defer wg.Done()
			body := mustJSON(t, activateRequest{VersionNumber: n})
			w := httptest.NewRecorder()
			r := withTenantAdminCtx(muxRequest(http.MethodPost,
				"/v1/tenants/default/stacks/hello/activate", body,
				map[string]string{"name": "hello"}), testTenant)
			c.handleActivateStack(w, r)
			if w.Code != http.StatusOK {
				errs <- fmt.Errorf("activate v%d: %d %s", n, w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent activate: %v", err)
	}

	// Whatever the final winner is, ops must hold exactly one row for
	// hello at scope 100 — never empty, never multiple.
	var opsCount int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM ops WHERE tenant_id = ? AND stack = ?`,
		testTenant, "hello").Scan(&opsCount); err != nil {
		t.Fatalf("count ops: %v", err)
	}
	if opsCount != 1 {
		t.Errorf("ops count after concurrent activates = %d, want 1 (no torn state)", opsCount)
	}

	// active_version must point to one of the three versions we
	// created — never NULL after at least one activate succeeded.
	var activeVersionID *int64
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT active_version FROM stacks WHERE tenant_id = ? AND name = ?`,
		testTenant, "hello").Scan(&activeVersionID); err != nil {
		t.Fatalf("read active_version: %v", err)
	}
	if activeVersionID == nil {
		t.Errorf("active_version is NULL after concurrent activates")
	}
}

// TestStackHierarchyVersioning verifies the activate flow is per-stack:
// activating a new version of `website` doesn't touch ops rows belonging
// to `website/canary`. The fallback semantics live in the processor (out
// of scope here); this test only confirms the materialisation boundary.
func TestStackHierarchyVersioning(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	// website v1 with one rule at scope 100.
	v := callCreateDraft(t, c, "website", "")
	callPutFiles(t, c, "website", v, []stackFile{
		{Path: "100/root.txcl", Content: `EXEC "http://example.com/root-v1"`},
	})
	callActivate(t, c, "website", v)

	// website/canary v1 with one rule at scope 200.
	v = callCreateDraft(t, c, "website/canary", "")
	callPutFiles(t, c, "website/canary", v, []stackFile{
		{Path: "200/override.txcl", Content: `EXEC "http://example.com/canary"`},
	})
	callActivate(t, c, "website/canary", v)

	// Push website v2 and activate; canary's row must be untouched.
	v = callCreateDraft(t, c, "website", "active")
	callPutFiles(t, c, "website", v, []stackFile{
		{Path: "100/root.txcl", Content: `EXEC "http://example.com/root-v2"`},
	})
	callActivate(t, c, "website", v)

	var canaryTxcl string
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT txcl FROM ops WHERE tenant_id = ? AND stack = ? AND scope = ?`,
		testTenant, "website/canary", 200).Scan(&canaryTxcl); err != nil {
		t.Fatalf("read canary: %v", err)
	}
	if canaryTxcl != `EXEC "http://example.com/canary"` {
		t.Errorf("canary txcl = %q, want unchanged after activating website v2", canaryTxcl)
	}

	var rootTxcl string
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT txcl FROM ops WHERE tenant_id = ? AND stack = ? AND scope = ?`,
		testTenant, "website", 100).Scan(&rootTxcl); err != nil {
		t.Fatalf("read root: %v", err)
	}
	if rootTxcl != `EXEC "http://example.com/root-v2"` {
		t.Errorf("website root txcl = %q, want v2", rootTxcl)
	}
}

// callPatchFile drives handlePatchDraftFile through the test mux setup.
// Returns the recorder so callers can branch on Code + Body.
func callPatchFile(c *Controller, stack string, n int64, path, content, baseHash string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(patchFileRequest{Path: path, Content: content, BaseHash: baseHash})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPatch,
		"/v1/tenants/default/stacks/"+stack+"/versions/"+strconv.FormatInt(n, 10)+"/files", body,
		map[string]string{"name": stack, "n": strconv.FormatInt(n, 10)}), testTenant)
	c.handlePatchDraftFile(w, r)
	return w
}

func callDeleteFile(c *Controller, stack string, n int64, path, baseHash string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(deleteFileRequest{Path: path, BaseHash: baseHash})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodDelete,
		"/v1/tenants/default/stacks/"+stack+"/versions/"+strconv.FormatInt(n, 10)+"/files", body,
		map[string]string{"name": stack, "n": strconv.FormatInt(n, 10)}), testTenant)
	c.handleDeleteDraftFile(w, r)
	return w
}

// readManifestHash is a tiny helper for tests that assert on manifest
// recompute side-effects.
func readManifestHash(t *testing.T, c *Controller, stack string, n int64) string {
	t.Helper()
	var stackID string
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT stack_id FROM stacks WHERE tenant_id = ? AND name = ?`,
		testTenant, stack).Scan(&stackID); err != nil {
		t.Fatalf("lookup stack: %v", err)
	}
	var mh string
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT manifest_hash FROM stack_versions WHERE stack_id = ? AND version_number = ?`,
		stackID, n).Scan(&mh); err != nil {
		t.Fatalf("read manifest_hash: %v", err)
	}
	return mh
}

// TestPatchUpdateHappy: PATCH an existing file with the correct
// base_hash. Server returns 200; file content + content_hash + the
// version's manifest_hash all reflect the update.
func TestPatchUpdateHappy(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/v1"`},
	})
	preHash := readManifestHash(t, c, "hello", n)

	// Compute the existing file's content_hash to use as base_hash.
	baseHash := sha256Hex(`EXEC "http://example.com/v1"`)
	newContent := `EXEC "http://example.com/v2"`
	w := callPatchFile(c, "hello", n, "100/main.txcl", newContent, baseHash)
	if w.Code != http.StatusOK {
		t.Fatalf("patch update: %d body=%s", w.Code, w.Body.String())
	}
	var resp patchFileResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	if resp.ContentHash != sha256Hex(newContent) {
		t.Errorf("response content_hash = %q, want sha256(newContent) = %q", resp.ContentHash, sha256Hex(newContent))
	}
	if resp.ManifestHash == preHash {
		t.Errorf("manifest_hash unchanged after PATCH; want recompute")
	}
	postHash := readManifestHash(t, c, "hello", n)
	if postHash != resp.ManifestHash {
		t.Errorf("DB manifest_hash %q != response %q", postHash, resp.ManifestHash)
	}

	// Confirm content lands.
	var got string
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT content FROM stack_files WHERE path = ? AND version_id = (SELECT version_id FROM stack_versions sv JOIN stacks s ON s.stack_id = sv.stack_id WHERE s.tenant_id = ? AND s.name = ? AND sv.version_number = ?)`,
		"100/main.txcl", testTenant, "hello", n).Scan(&got); err != nil {
		t.Fatalf("read content: %v", err)
	}
	if got != newContent {
		t.Errorf("stored content = %q, want %q", got, newContent)
	}
}

// TestPatchCreateHappy: PATCH a path that doesn't exist yet with an
// empty base_hash. Server creates the row and returns 200.
func TestPatchCreateHappy(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/keep.txcl", Content: `EXEC "http://example.com/keep"`},
	})

	w := callPatchFile(c, "hello", n, "200/added.txcl", `EXEC "http://example.com/added"`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("patch create: %d body=%s", w.Code, w.Body.String())
	}
	// Verify both files now coexist.
	var count int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM stack_files WHERE version_id = (SELECT version_id FROM stack_versions sv JOIN stacks s ON s.stack_id = sv.stack_id WHERE s.tenant_id = ? AND s.name = ? AND sv.version_number = ?)`,
		testTenant, "hello", n).Scan(&count); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if count != 2 {
		t.Errorf("stack_files count after PATCH-create = %d, want 2", count)
	}
}

// TestPatchStaleBaseHash: 409 when the caller's base_hash doesn't
// match the current hash.
func TestPatchStaleBaseHash(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/v1"`},
	})

	w := callPatchFile(c, "hello", n, "100/main.txcl", `EXEC "http://example.com/v2"`, "deadbeef-not-the-real-hash")
	if w.Code != http.StatusConflict {
		t.Errorf("got %d, want 409 base_hash_mismatch body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("base_hash_mismatch")) {
		t.Errorf("body did not carry base_hash_mismatch error code: %s", w.Body.String())
	}
}

// TestPatchFileAlreadyExists: 409 when caller PATCHes with empty
// base_hash against a path that already exists.
func TestPatchFileAlreadyExists(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/v1"`},
	})

	w := callPatchFile(c, "hello", n, "100/main.txcl", `EXEC "http://example.com/v2"`, "")
	if w.Code != http.StatusConflict {
		t.Errorf("got %d, want 409 file_already_exists body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("file_already_exists")) {
		t.Errorf("body did not carry file_already_exists error code: %s", w.Body.String())
	}
}

// TestPatchFileMissingIs404: 404 (not 409) when the caller passes
// non-empty base_hash against a path that doesn't exist. The 404/409
// split is intentional — 404 = resource isn't there.
func TestPatchFileMissingIs404(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/v1"`},
	})

	w := callPatchFile(c, "hello", n, "999/ghost.txcl", `EXEC "http://example.com/x"`, "anyhash")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 file_not_found body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("file_not_found")) {
		t.Errorf("body did not carry file_not_found error code: %s", w.Body.String())
	}
}

// TestPatchNonDraft: 409 once the version has been activated.
func TestPatchNonDraft(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/v1"`},
	})
	callActivate(t, c, "hello", n)

	w := callPatchFile(c, "hello", n, "100/main.txcl",
		`EXEC "http://example.com/sneaky"`, sha256Hex(`EXEC "http://example.com/v1"`))
	if w.Code != http.StatusConflict {
		t.Errorf("got %d, want 409 version_not_draft body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("version_not_draft")) {
		t.Errorf("body did not carry version_not_draft error code: %s", w.Body.String())
	}
}

// TestStackFilePathValidation: the validator rejects bad shapes
// uniformly at PATCH and at PUT-files (regression-proofing — Phase 2a
// pulled the same validator into PUT).
func TestStackFilePathValidation(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")

	badPaths := []string{
		"/absolute.txcl",
		"..",
		"a/../b.txcl",
		"a//b.txcl",
		"./a.txcl",
		"foo.txt",           // wrong extension
		"100/main",          // no extension
		"100/random.json",   // .json with non-mock-* basename
		"100/he%llo.txcl",   // bad rule-name char
		"100/bad name.txcl", // whitespace in rule name
		"100/..foo.txcl",    // leading dots in rule name
	}
	for _, p := range badPaths {
		w := callPatchFile(c, "hello", n, p, "content", "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("PATCH path=%q: got %d, want 400 invalid_path; body=%s", p, w.Code, w.Body.String())
		}
		// And at PUT-files (same battery, via the shared helper).
		body := mustJSON(t, putFilesRequest{Files: []stackFile{
			{Path: p, Content: "content"},
		}})
		wp := httptest.NewRecorder()
		r := withTenantAdminCtx(muxRequest(http.MethodPut,
			"/v1/tenants/default/stacks/hello/versions/1/files", body,
			map[string]string{"name": "hello", "n": "1"}), testTenant)
		c.handlePutDraftFiles(wp, r)
		if wp.Code != http.StatusBadRequest {
			t.Errorf("PUT path=%q: got %d, want 400 invalid_path; body=%s", p, wp.Code, wp.Body.String())
		}
	}

	// The well-known mock filenames are allowed.
	for _, p := range []string{
		"100/main.txcl",
		"100/ok-name.txcl",
		"100/Op_1.txcl",
		"200/mock-request.json",
		"200/mock-response.json",
	} {
		w := callPatchFile(c, "hello", n, p, "content", "")
		if w.Code == http.StatusBadRequest {
			t.Errorf("PATCH path=%q rejected as invalid; body=%s", p, w.Body.String())
		}
	}
}

// TestDeleteHappy: DELETE existing file with correct base_hash → 200.
func TestDeleteHappy(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/x"`},
		{Path: "200/audit.txcl", Content: `EXEC "http://example.com/audit"`},
	})

	w := callDeleteFile(c, "hello", n, "100/main.txcl", sha256Hex(`EXEC "http://example.com/x"`))
	if w.Code != http.StatusOK {
		t.Fatalf("delete: %d body=%s", w.Code, w.Body.String())
	}
	var resp deleteFileResponse
	decodeJSON(t, w.Body.Bytes(), &resp)
	if !resp.Deleted {
		t.Errorf("response.deleted = false, want true")
	}
	if resp.Path != "100/main.txcl" {
		t.Errorf("response.path = %q, want 100/main.txcl", resp.Path)
	}

	// File is gone; the other one remains.
	var count int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM stack_files WHERE version_id = (SELECT version_id FROM stack_versions sv JOIN stacks s ON s.stack_id = sv.stack_id WHERE s.tenant_id = ? AND s.name = ? AND sv.version_number = ?)`,
		testTenant, "hello", n).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("stack_files count after DELETE = %d, want 1", count)
	}
}

// TestDeleteMissingIs404: DELETE a path that isn't there → 404.
func TestDeleteMissingIs404(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/x"`},
	})

	w := callDeleteFile(c, "hello", n, "999/ghost.txcl", "anyhash")
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404 file_not_found; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("file_not_found")) {
		t.Errorf("body did not carry file_not_found: %s", w.Body.String())
	}
}

// TestDeleteStaleBaseHash: DELETE with wrong hash → 409.
func TestDeleteStaleBaseHash(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/x"`},
	})

	w := callDeleteFile(c, "hello", n, "100/main.txcl", "deadbeef")
	if w.Code != http.StatusConflict {
		t.Errorf("got %d, want 409 base_hash_mismatch; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("base_hash_mismatch")) {
		t.Errorf("body did not carry base_hash_mismatch: %s", w.Body.String())
	}
	// File is still there.
	var count int
	if err := c.pu.RuntimeDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM stack_files WHERE path = ? AND version_id = (SELECT version_id FROM stack_versions sv JOIN stacks s ON s.stack_id = sv.stack_id WHERE s.tenant_id = ? AND s.name = ? AND sv.version_number = ?)`,
		"100/main.txcl", testTenant, "hello", n).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("file gone after rejected DELETE: count=%d", count)
	}
}

// TestDeleteRequiresBaseHash: empty base_hash → 400 missing_base_hash.
func TestDeleteRequiresBaseHash(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/x"`},
	})

	w := callDeleteFile(c, "hello", n, "100/main.txcl", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400 missing_base_hash; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("missing_base_hash")) {
		t.Errorf("body did not carry missing_base_hash: %s", w.Body.String())
	}
}

// TestDeleteNonDraft: DELETE against a superseded version → 409.
func TestDeleteNonDraft(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/main.txcl", Content: `EXEC "http://example.com/x"`},
	})
	callActivate(t, c, "hello", n)

	w := callDeleteFile(c, "hello", n, "100/main.txcl", sha256Hex(`EXEC "http://example.com/x"`))
	if w.Code != http.StatusConflict {
		t.Errorf("got %d, want 409 version_not_draft; body=%s", w.Code, w.Body.String())
	}
}

// TestActivateAfterPatchAndDelete: chain mutations on a draft, then
// activate; the materialised ops table reflects the final state.
func TestActivateAfterPatchAndDelete(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/keep.txcl", Content: `EXEC "http://example.com/keep"`},
		{Path: "100/will-remove.txcl", Content: `EXEC "http://example.com/old"`},
		{Path: "200/will-change.txcl", Content: `EXEC "http://example.com/before"`},
	})

	// Update will-change.
	w1 := callPatchFile(c, "hello", n, "200/will-change.txcl",
		`EXEC "http://example.com/after"`, sha256Hex(`EXEC "http://example.com/before"`))
	if w1.Code != http.StatusOK {
		t.Fatalf("patch update: %d %s", w1.Code, w1.Body.String())
	}
	// Add a new file.
	w2 := callPatchFile(c, "hello", n, "300/added.txcl",
		`EXEC "http://example.com/new"`, "")
	if w2.Code != http.StatusOK {
		t.Fatalf("patch create: %d %s", w2.Code, w2.Body.String())
	}
	// Delete will-remove.
	w3 := callDeleteFile(c, "hello", n, "100/will-remove.txcl",
		sha256Hex(`EXEC "http://example.com/old"`))
	if w3.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w3.Code, w3.Body.String())
	}

	callActivate(t, c, "hello", n)

	// ops should now have: 100/keep, 200/will-change (updated), 300/added.
	rows, err := c.pu.RuntimeDB.QueryContext(context.Background(),
		`SELECT scope, name, txcl FROM ops WHERE tenant_id = ? AND stack = ? ORDER BY scope, name`,
		testTenant, "hello")
	if err != nil {
		t.Fatalf("query ops: %v", err)
	}
	defer rows.Close()
	type opRow struct {
		Scope int
		Name  string
		Txcl  string
	}
	var got []opRow
	for rows.Next() {
		var o opRow
		if err := rows.Scan(&o.Scope, &o.Name, &o.Txcl); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, o)
	}
	want := []opRow{
		{100, "keep", `EXEC "http://example.com/keep"`},
		{200, "will-change", `EXEC "http://example.com/after"`},
		{300, "added", `EXEC "http://example.com/new"`},
	}
	if len(got) != len(want) {
		t.Fatalf("ops rows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestPatchUpdatesManifestHash: confirms the version's manifest_hash
// changes on every PATCH and matches a fresh recompute, so the diff
// endpoint's manifest-hash short-circuit stays correct.
func TestPatchUpdatesManifestHash(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	n := callCreateDraft(t, c, "hello", "")
	callPutFiles(t, c, "hello", n, []stackFile{
		{Path: "100/a.txcl", Content: `EXEC "http://example.com/a"`},
		{Path: "100/b.txcl", Content: `EXEC "http://example.com/b"`},
	})
	pre := readManifestHash(t, c, "hello", n)

	w := callPatchFile(c, "hello", n, "100/a.txcl",
		`EXEC "http://example.com/a2"`, sha256Hex(`EXEC "http://example.com/a"`))
	if w.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", w.Code, w.Body.String())
	}
	post := readManifestHash(t, c, "hello", n)
	if post == pre || post == "" {
		t.Errorf("manifest_hash didn't change (pre=%q post=%q)", pre, post)
	}

	// A second PATCH with different content produces a third hash.
	w2 := callPatchFile(c, "hello", n, "100/a.txcl",
		`EXEC "http://example.com/a3"`, sha256Hex(`EXEC "http://example.com/a2"`))
	if w2.Code != http.StatusOK {
		t.Fatalf("patch2: %d %s", w2.Code, w2.Body.String())
	}
	post2 := readManifestHash(t, c, "hello", n)
	if post2 == post {
		t.Errorf("second PATCH didn't change manifest_hash (%q)", post2)
	}
}

// TestValidateStackName locks the boot/* namespace reservation. A
// tenant-owned `boot/<x>` stack would otherwise be swept into the
// untenanted `boot/%/0` ingress fallback and run for traffic that
// isn't theirs (cross-tenant escalation). The check is
// case-insensitive (SQLite LIKE is ASCII-case-insensitive) and `%` is
// barred outright (it's the LIKE wildcard).
func TestValidateStackName(t *testing.T) {
	const realTenant = "tnt_default"

	// boot/* is rejected for any non-system tenant.
	reservedForTenant := []string{
		"boot",
		"boot/web",
		"boot/anything/deep",
		"BOOT/web", // SQLite LIKE is case-insensitive
		"Boot/Web", // mixed case
	}
	for _, n := range reservedForTenant {
		if err := validateStackName(n, realTenant); err == nil {
			t.Errorf("validateStackName(%q, realTenant) = nil, want rejection", n)
		}
		// But the system tenant owns boot/* and may create it.
		if err := validateStackName(n, tenants.SystemTenantID); err != nil {
			t.Errorf("validateStackName(%q, _sys) = %v, want nil (system owns boot/*)", n, err)
		}
	}

	// `%` is rejected for everyone, system tenant included.
	for _, n := range []string{"web%", "a%b"} {
		if err := validateStackName(n, realTenant); err == nil {
			t.Errorf("validateStackName(%q, realTenant) = nil, want rejection", n)
		}
		if err := validateStackName(n, tenants.SystemTenantID); err == nil {
			t.Errorf("validateStackName(%q, _sys) = nil, want rejection (%% is never valid)", n)
		}
	}

	ok := []string{
		"web",
		"website/canary", // multi-segment overlay trees are legal
		"my_stack",       // underscores are common and allowed
		"booted",         // only exact `boot` / `boot/` prefix is reserved
		"reboot/x",
		"tenant-a/web",
	}
	for _, n := range ok {
		if err := validateStackName(n, realTenant); err != nil {
			t.Errorf("validateStackName(%q, realTenant) = %v, want nil", n, err)
		}
	}

	// Charset/segment rule (shared via opname): traversal/whitespace/
	// empty-segment names are rejected for everyone, including _sys.
	for _, n := range []string{
		"", "..", ".", "../x", "a b", "a.b", "/x", "x/", "a//b",
	} {
		if err := validateStackName(n, realTenant); err == nil {
			t.Errorf("validateStackName(%q, realTenant) = nil, want rejection", n)
		}
		if err := validateStackName(n, tenants.SystemTenantID); err == nil {
			t.Errorf("validateStackName(%q, _sys) = nil, want rejection", n)
		}
	}
}

// TestReservedStackNameRejectedAtCreate proves the guard is wired into
// the create-draft handler: a reserved name never even vivifies a
// stacks row.
func TestReservedStackNameRejectedAtCreate(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	body := mustJSON(t, createDraftRequest{})
	w := httptest.NewRecorder()
	r := withTenantAdminCtx(muxRequest(http.MethodPost,
		"/v1/tenants/default/stacks/boot/web/draft", body,
		map[string]string{"name": "boot/web"}), testTenant)
	c.handleCreateDraft(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("create draft boot/web: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("reserved_stack_name")) {
		t.Errorf("expected reserved_stack_name error, got %s", w.Body.String())
	}

	var n int
	if err := c.pu.RuntimeDB.QueryRow(
		`SELECT COUNT(*) FROM stacks WHERE name = ?`, "boot/web").Scan(&n); err != nil {
		t.Fatalf("count stacks: %v", err)
	}
	if n != 0 {
		t.Errorf("reserved name vivified %d stacks rows, want 0", n)
	}
}
