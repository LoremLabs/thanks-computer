package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// PutDraftFiles' manage scope is what makes data opt-in: "code" replaces rules +
// FILES and carries the store-seed packs forward; "data" replaces the packs and
// carries the code forward. (The draft is a clone of the active version, so the
// unmanaged category is already present and must survive the replace.)
func TestPutDraftFilesManageScope(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})

	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES ('stk','tnt_default','recs','t')`); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash)
		 VALUES (1,'stk',1,'draft','test','t','')`); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	// The cloned draft starts with a rule + a pack.
	seedVersion(t, c, 1, map[string]string{
		"100/route.txcl":      "NOOP",
		"VECTORS/books.jsonl": `{"id":"a","vector":[1,0]}`,
	})

	put := func(manage string, files []map[string]string) int {
		body, _ := json.Marshal(map[string]any{"manage": manage, "files": files})
		req := mux.SetURLVars(
			withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/v1/tenants/default/stacks/recs/versions/1/files", bytes.NewReader(body)), "tnt_default"),
			map[string]string{"name": "recs", "n": "1"})
		rec := httptest.NewRecorder()
		c.handlePutDraftFiles(rec, req)
		return rec.Code
	}

	// manage="code": replace the rule, preserve the pack.
	if code := put("code", []map[string]string{{"path": "200/new.txcl", "content": "NOOP"}}); code != http.StatusOK {
		t.Fatalf("put code: status %d", code)
	}
	got := versionPaths(t, c, 1)
	if !equalSet(got, []string{"200/new.txcl", "VECTORS/books.jsonl"}) {
		t.Fatalf("after manage=code: %v want [200/new.txcl VECTORS/books.jsonl] (pack carried forward, old rule replaced)", got)
	}

	// manage="data": replace the pack, preserve the (now-updated) code.
	if code := put("data", []map[string]string{{"path": "VECTORS/more.jsonl", "content": `{"id":"b","vector":[0,1]}`}}); code != http.StatusOK {
		t.Fatalf("put data: status %d", code)
	}
	got = versionPaths(t, c, 1)
	if !equalSet(got, []string{"200/new.txcl", "VECTORS/more.jsonl"}) {
		t.Fatalf("after manage=data: %v want [200/new.txcl VECTORS/more.jsonl] (code carried forward, old pack replaced)", got)
	}

	// manage="all" (default): replace everything.
	if code := put("all", []map[string]string{{"path": "300/only.txcl", "content": "NOOP"}}); code != http.StatusOK {
		t.Fatalf("put all: status %d", code)
	}
	got = versionPaths(t, c, 1)
	if !equalSet(got, []string{"300/only.txcl"}) {
		t.Fatalf("after manage=all: %v want [300/only.txcl] (full replace)", got)
	}
}

// handleGetStack returns a CODE-only manifest hash (excluding VECTORS/+KV/
// packs) so the code-only `apply` no-op short-circuit works on a pack-bearing
// stack. It must match the manifest of just the code files and differ from the
// all-files manifest.
func TestGetStackCodeManifestHash(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})

	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES ('stk','tnt_default','recs',1,'t')`); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash)
		 VALUES (1,'stk',1,'superseded','test','t','allfiles')`); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	seedVersion(t, c, 1, map[string]string{
		"100/route.txcl":      "NOOP",
		"VECTORS/books.jsonl": `{"id":"a","vector":[1,0]}`,
	})

	rec := mux.SetURLVars(
		withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/stacks/recs", nil), "tnt_default"),
		map[string]string{"name": "recs"})
	rr := httptest.NewRecorder()
	c.handleGetStack(rr, rec)
	if rr.Code != http.StatusOK {
		t.Fatalf("get stack: status %d body %s", rr.Code, rr.Body.String())
	}
	var got struct {
		ManifestHash     string `json:"manifest_hash"`
		CodeManifestHash string `json:"code_manifest_hash"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	wantCode := computeManifestHash([]stackFile{{Path: "100/route.txcl", ContentHash: sha256Hex("NOOP")}})
	if got.CodeManifestHash != wantCode {
		t.Fatalf("CodeManifestHash = %q, want %q (code files only)", got.CodeManifestHash, wantCode)
	}
	wantAll := computeManifestHash([]stackFile{
		{Path: "100/route.txcl", ContentHash: sha256Hex("NOOP")},
		{Path: "VECTORS/books.jsonl", ContentHash: sha256Hex(`{"id":"a","vector":[1,0]}`)},
	})
	if got.CodeManifestHash == wantAll {
		t.Fatal("CodeManifestHash must exclude the pack (it equals the all-files manifest)")
	}
}

func versionPaths(t *testing.T, c *Controller, versionID int64) []string {
	t.Helper()
	rows, err := c.pu.RuntimeDB.QueryContext(context.Background(),
		`SELECT path FROM stack_files WHERE version_id = ? ORDER BY path`, versionID)
	if err != nil {
		t.Fatalf("query paths: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func equalSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}
