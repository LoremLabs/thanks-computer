package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/computesrc"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

const srcDigest = "3cee8f8dc76d5e1c04156da5ec03fe2dc8f347f2b742386e19e02d55235683d2"

func TestValidateStackFilePathComputes(t *testing.T) {
	if err := validateStackFilePath(computesrc.Path(srcDigest)); err != nil {
		t.Fatalf("valid COMPUTES/ row refused: %v", err)
	}
	for _, bad := range []string{
		"COMPUTES/" + srcDigest + ".js",
		"COMPUTES/abc.json",
		"COMPUTES/x/" + srcDigest + ".json",
		"computes/" + srcDigest + ".json",
	} {
		if err := validateStackFilePath(bad); err == nil {
			t.Errorf("validateStackFilePath(%q) accepted", bad)
		}
	}
}

// computeSourceFixture seeds a stack "site" (draft v1) for tnt_default with a
// file-backed CAS, and returns the encoded bundle and its hash.
func computeSourceFixture(t *testing.T) (*Controller, []byte, string) {
	t.Helper()
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	wireDatasetStores(t, c)
	for _, q := range []string{
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES ('stk','tnt_default','site','t')`,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash)
		 VALUES (1,'stk',1,'draft','test','t','')`,
	} {
		if _, err := c.pu.RuntimeDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	data, hash, err := computesrc.Bundle{V: 1, Entry: "plan.js", Files: []computesrc.File{
		{Path: "plan.js", Content: "export default op(({input}) => input)"},
	}}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return c, data, hash
}

func putDraftFiles(c *Controller, files []map[string]string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"files": files, "manage": "code"})
	req := mux.SetURLVars(
		withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/v1/tenants/default/stacks/site/versions/1/files", bytes.NewReader(body)), "tnt_default"),
		map[string]string{"name": "site", "n": "1"})
	rec := httptest.NewRecorder()
	c.handlePutDraftFiles(rec, req)
	return rec
}

func getComputeSource(c *Controller, tenantID, stack, digest string) *httptest.ResponseRecorder {
	req := mux.SetURLVars(
		withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/x/stacks/"+stack+"/computes/sha256/"+digest, nil), tenantID),
		map[string]string{"name": stack, "digest": digest})
	rec := httptest.NewRecorder()
	c.handleGetComputeSource(rec, req)
	return rec
}

// A COMPUTES/ row is fingerprint-only: inline content is refused, an absent
// bundle is refused, a resident one is stored as hash-only.
func TestPutDraftFilesComputeSourceRow(t *testing.T) {
	ctx := context.Background()
	c, data, hash := computeSourceFixture(t)
	path := computesrc.Path(srcDigest)

	if rec := putDraftFiles(c, []map[string]string{{"path": path, "content": string(data)}}); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "cas_required") {
		t.Fatalf("inline COMPUTES/: %d %s", rec.Code, rec.Body.String())
	}
	row := map[string]string{"path": path, "content_hash": hash, "encoding": "cas"}
	if rec := putDraftFiles(c, []map[string]string{row}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("absent bundle: %d %s", rec.Code, rec.Body.String())
	}
	if err := c.fcas.Put(ctx, hash, data); err != nil {
		t.Fatal(err)
	}
	if rec := putDraftFiles(c, []map[string]string{row}); rec.Code != http.StatusOK {
		t.Fatalf("resident bundle: %d %s", rec.Code, rec.Body.String())
	}
	var content, stored string
	if err := c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT content, content_hash FROM stack_files WHERE version_id=1 AND path=?`, path).Scan(&content, &stored); err != nil {
		t.Fatal(err)
	}
	if content != "" || stored != hash {
		t.Fatalf("stored row content=%q hash=%q", content, stored)
	}

	// Neither the ops view nor a full read inlines a bundle: the row comes
	// back as its hash (marked "cas" when bodies are requested, as for BLOBS/).
	for mode, enc := range map[contentMode]string{contentOps: "", contentAll: "cas"} {
		files, err := c.loadVersionFiles(ctx, 1, mode)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 1 || files[0].Encoding != enc || files[0].Content != "" || files[0].ContentHash != hash {
			t.Fatalf("mode %d row = %+v", mode, files)
		}
	}

	// The fleet artifact ships the row as its fingerprint — an unclassified
	// path would travel as {path, content:""} and land as sha256("").
	art, err := c.readStackFilesForArtifact(ctx, "tnt_default", "site", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(art) != 1 || art[0].Path != path || art[0].ContentHash != hash || art[0].Content != "" {
		t.Fatalf("fleet artifact files = %+v", art)
	}
}

// The source is served only through the caller's own stack's row.
func TestGetComputeSourceIsFenced(t *testing.T) {
	ctx := context.Background()
	c, data, hash := computeSourceFixture(t)
	if err := c.fcas.Put(ctx, hash, data); err != nil {
		t.Fatal(err)
	}
	if rec := putDraftFiles(c, []map[string]string{{"path": computesrc.Path(srcDigest), "content_hash": hash, "encoding": "cas"}}); rec.Code != http.StatusOK {
		t.Fatalf("seed row: %d %s", rec.Code, rec.Body.String())
	}

	rec := getComputeSource(c, "tnt_default", "site", srcDigest)
	if rec.Code != http.StatusOK {
		t.Fatalf("own stack: %d %s", rec.Code, rec.Body.String())
	}
	var got computeSourceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Entry != "plan.js" || got.Version != 1 || len(got.Files) != 1 || got.Stack != "site" {
		t.Fatalf("response = %+v", got)
	}

	// Another stack of the same tenant doesn't carry it.
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES ('stk2','tnt_default','other','t')`); err != nil {
		t.Fatal(err)
	}
	if rec := getComputeSource(c, "tnt_default", "other", srcDigest); rec.Code != http.StatusNotFound ||
		!strings.Contains(rec.Body.String(), "no_source") {
		t.Fatalf("other stack: %d %s", rec.Code, rec.Body.String())
	}
	// Another tenant's stack of the same name can't reach it either.
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES ('stk3','tnt_sys','site','t')`); err != nil {
		t.Fatal(err)
	}
	if rec := getComputeSource(c, "tnt_sys", "site", srcDigest); rec.Code != http.StatusNotFound {
		t.Fatalf("other tenant: %d %s", rec.Code, rec.Body.String())
	}
	// A digest the stack never recorded.
	if rec := getComputeSource(c, "tnt_default", "site", strings.Repeat("0", 64)); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown digest: %d", rec.Code)
	}
}

func TestMissingBlobs(t *testing.T) {
	ctx := context.Background()
	c, data, hash := computeSourceFixture(t)
	if err := c.fcas.Put(ctx, hash, data); err != nil {
		t.Fatal(err)
	}
	absent := strings.Repeat("a", 64)
	post := func(body string) *httptest.ResponseRecorder {
		req := withTenantAdminCtx(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/blobs/missing", strings.NewReader(body)), "tnt_default")
		rec := httptest.NewRecorder()
		c.handleMissingBlobs(rec, req)
		return rec
	}

	rec := post(`{"hashes":["` + hash + `","` + absent + `","` + absent + `"]}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"missing":["`+absent+`"]}` {
		t.Fatalf("probe: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"hashes":[]}`); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"missing":[]}` {
		t.Fatalf("empty probe: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(`{"hashes":["nope"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: %d", rec.Code)
	}
	many := make([]string, maxMissingBlobsBatch+1)
	for i := range many {
		many[i] = absent
	}
	body, _ := json.Marshal(map[string][]string{"hashes": many})
	if rec := post(string(body)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the batch cap: %d", rec.Code)
	}
}
