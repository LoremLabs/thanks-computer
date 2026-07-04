package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dataset"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
)

func TestValidateStackFilePathDatasets(t *testing.T) {
	valid := []string{
		"DATASETS/books.sqlite",
		"DATASETS/books.yaml",
		"DATASETS/geo-ip2.sqlite",
	}
	for _, p := range valid {
		if err := validateStackFilePath(p); err != nil {
			t.Errorf("validateStackFilePath(%q) = %v, want nil", p, err)
		}
	}
	invalid := []string{
		"DATASETS/books.db",            // wrong extension
		"DATASETS/nested/books.sqlite", // no nesting
		"DATASETS/.sqlite",             // empty name
		"DATASETS",                     // bare dir
		"datasets/books.sqlite",        // case-sensitive channel
	}
	for _, p := range invalid {
		if err := validateStackFilePath(p); err == nil {
			t.Errorf("validateStackFilePath(%q) = nil, want error", p)
		}
	}
}

func TestDatasetPairError(t *testing.T) {
	if me := datasetPairError([]string{"DATASETS/books.sqlite", "DATASETS/books.yaml", "100/x.txcl"}); me != nil {
		t.Fatalf("complete pair flagged: %+v", me)
	}
	if me := datasetPairError([]string{"100/x.txcl", "FILES/a.png"}); me != nil {
		t.Fatalf("dataset-free version flagged: %+v", me)
	}
	me := datasetPairError([]string{"DATASETS/books.sqlite"})
	if me == nil || me.code != "dataset_pair_incomplete" {
		t.Fatalf("orphan artifact not flagged: %+v", me)
	}
	me = datasetPairError([]string{"DATASETS/books.yaml"})
	if me == nil || me.code != "dataset_pair_incomplete" {
		t.Fatalf("orphan manifest not flagged: %+v", me)
	}
}

// dsFixture builds a small SQLite artifact and returns (bytes, hash).
func dsFixture(t *testing.T) ([]byte, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "books.sqlite")
	db, err := sql.Open("sqlite3", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE books (isbn13 TEXT PRIMARY KEY, title TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO books VALUES ('9780000000001','Angels with Dirty Faces')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

// wireDatasetStores attaches a real file-backed CAS + dataset cache.
func wireDatasetStores(t *testing.T, c *Controller) *filestore.FileStore {
	t.Helper()
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c.SetFileCAS(fs)
	dc, err := dataset.NewCache(t.TempDir(), fs, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dc.Close)
	c.SetDatasetCache(dc)
	return fs
}

// TestPutDraftFilesCasRow pins the fingerprint-only row contract: a row may
// reference only bytes the CAS actually holds.
func TestPutDraftFilesCasRow(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	fs := wireDatasetStores(t, c)

	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES ('stk','tnt_default','recs','t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash)
		 VALUES (1,'stk',1,'draft','test','t','')`); err != nil {
		t.Fatal(err)
	}

	data, hash := dsFixture(t)
	put := func(files []map[string]string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"files": files})
		req := mux.SetURLVars(
			withTenantAdminCtx(httptest.NewRequest(http.MethodPut, "/v1/tenants/default/stacks/recs/versions/1/files", bytes.NewReader(body)), "tnt_default"),
			map[string]string{"name": "recs", "n": "1"})
		rec := httptest.NewRecorder()
		c.handlePutDraftFiles(rec, req)
		return rec
	}
	casRow := map[string]string{"path": "DATASETS/books.sqlite", "content_hash": hash, "encoding": "cas"}
	manifestRow := map[string]string{"path": "DATASETS/books.yaml", "content": "queries:\n  by_isbn:\n    sql: SELECT title FROM books WHERE isbn13 = ?\n"}

	// Blob absent → the whole PUT refuses.
	if rec := put([]map[string]string{casRow, manifestRow}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "missing_blob") {
		t.Fatalf("absent blob: status %d body %s", rec.Code, rec.Body.String())
	}
	// Stream the blob, retry: accepted, stored fingerprint-only.
	if err := fs.Put(ctx, hash, data); err != nil {
		t.Fatal(err)
	}
	if rec := put([]map[string]string{casRow, manifestRow}); rec.Code != http.StatusOK {
		t.Fatalf("cas row with resident blob: status %d body %s", rec.Code, rec.Body.String())
	}
	var content, storedHash string
	if err := c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT content, content_hash FROM stack_files WHERE version_id=1 AND path='DATASETS/books.sqlite'`).
		Scan(&content, &storedHash); err != nil {
		t.Fatal(err)
	}
	if content != "" || storedHash != hash {
		t.Fatalf("stored row: content %d bytes, hash %q (want empty content + supplied hash)", len(content), storedHash)
	}

	// A cas row carrying content is a client bug.
	bad := map[string]string{"path": "DATASETS/books.sqlite", "content": "x", "content_hash": hash, "encoding": "cas"}
	if rec := put([]map[string]string{bad, manifestRow}); rec.Code != http.StatusBadRequest {
		t.Fatalf("cas row with content: status %d", rec.Code)
	}
	// Malformed hash refused at the write boundary.
	ugly := map[string]string{"path": "DATASETS/books.sqlite", "content_hash": "nope", "encoding": "cas"}
	if rec := put([]map[string]string{ugly, manifestRow}); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed hash: status %d", rec.Code)
	}
}

// TestDeepValidateDatasets pins the activation gate end-to-end at the admin
// layer: CAS presence, manifest parse, query preparation against the real
// artifact (through the CAS-resolve + cache path).
func TestDeepValidateDatasets(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	fs := wireDatasetStores(t, c)

	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES ('stk','tnt_default','recs','t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash)
		 VALUES (1,'stk',1,'draft','test','t','')`); err != nil {
		t.Fatal(err)
	}

	data, hash := dsFixture(t)
	seedMember := func(manifest string) {
		if _, err := c.pu.RuntimeDB.ExecContext(ctx, `DELETE FROM stack_files WHERE version_id=1`); err != nil {
			t.Fatal(err)
		}
		// Artifact rides fingerprint-only (fleet/blob-endpoint shape).
		if _, err := c.pu.RuntimeDB.ExecContext(ctx,
			`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (1,'DATASETS/books.sqlite','',?)`,
			hash); err != nil {
			t.Fatal(err)
		}
		seedVersion(t, c, 1, map[string]string{"DATASETS/books.yaml": manifest})
	}

	good := "queries:\n  by_isbn:\n    sql: SELECT title FROM books WHERE isbn13 = ?\n"

	// Artifact not in CAS yet → named issue.
	seedMember(good)
	issues := c.deepValidateDatasets(ctx, 1)
	if len(issues) != 1 || !strings.Contains(issues[0].Err, "not in the CAS") {
		t.Fatalf("missing artifact: %+v", issues)
	}

	// Blob resident → clean.
	if err := fs.Put(ctx, hash, data); err != nil {
		t.Fatal(err)
	}
	if issues := c.deepValidateDatasets(ctx, 1); len(issues) != 0 {
		t.Fatalf("good pair flagged: %+v", issues)
	}

	// Query against a missing column fails preparation.
	seedMember("queries:\n  broken:\n    sql: SELECT nope FROM books\n")
	issues = c.deepValidateDatasets(ctx, 1)
	if len(issues) != 1 || !strings.Contains(issues[0].Err, "broken") {
		t.Fatalf("bad schema query: %+v", issues)
	}

	// A write masquerading as a query is refused by the authorizer at prepare.
	seedMember("queries:\n  evil:\n    sql: DELETE FROM books\n")
	if issues = c.deepValidateDatasets(ctx, 1); len(issues) != 1 {
		t.Fatalf("write query: %+v", issues)
	}

	// Garbage manifest.
	seedMember("querys: {}\n")
	if issues = c.deepValidateDatasets(ctx, 1); len(issues) != 1 {
		t.Fatalf("garbage manifest: %+v", issues)
	}

	// No datasets at all → fast nil.
	if _, err := c.pu.RuntimeDB.ExecContext(ctx, `DELETE FROM stack_files WHERE version_id=1`); err != nil {
		t.Fatal(err)
	}
	seedVersion(t, c, 1, map[string]string{"100/x.txcl": "NOOP"})
	if issues := c.deepValidateDatasets(ctx, 1); issues != nil {
		t.Fatalf("dataset-free version produced issues: %+v", issues)
	}
}

// TestBlobEndpoints pins the streaming blob plane: HEAD probe semantics,
// verified PUT, hash-mismatch refusal, size cap, and required length.
func TestBlobEndpoints(t *testing.T) {
	c := newTestController(t, config.Config{DatasetMaxFileBytes: 1 << 20})
	fs := wireDatasetStores(t, c)
	_ = fs

	data := []byte("blob plane bytes")
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	do := func(method, hashArg string, body io.Reader) *httptest.ResponseRecorder {
		req := mux.SetURLVars(
			withTenantAdminCtx(httptest.NewRequest(method, "/v1/tenants/default/blobs/sha256/"+hashArg, body), "tnt_default"),
			map[string]string{"hash": hashArg})
		rec := httptest.NewRecorder()
		if method == http.MethodHead {
			c.handleHeadBlob(rec, req)
		} else {
			c.handlePutBlob(rec, req)
		}
		return rec
	}

	if rec := do(http.MethodHead, hash, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("HEAD absent: %d", rec.Code)
	}
	if rec := do(http.MethodPut, hash, bytes.NewReader(data)); rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodHead, hash, nil); rec.Code != http.StatusOK {
		t.Fatalf("HEAD resident: %d", rec.Code)
	}
	// Idempotent re-PUT.
	if rec := do(http.MethodPut, hash, bytes.NewReader(data)); rec.Code != http.StatusOK {
		t.Fatalf("re-PUT: %d", rec.Code)
	}
	// Body that doesn't hash to the URL → 422, nothing stored.
	other := sha256.Sum256([]byte("other"))
	otherHash := hex.EncodeToString(other[:])
	if rec := do(http.MethodPut, otherHash, bytes.NewReader(data)); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch PUT: %d %s", rec.Code, rec.Body.String())
	}
	if ok, _ := fs.Exists(context.Background(), otherHash); ok {
		t.Fatal("mismatched blob became visible")
	}
	// Over the configured cap → 413 before reading.
	big := bytes.NewReader(make([]byte, (1<<20)+1))
	if rec := do(http.MethodPut, hash, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized PUT: %d", rec.Code)
	}
	// No Content-Length (chunked) → 411.
	chunked := struct{ io.Reader }{bytes.NewReader(data)}
	if rec := do(http.MethodPut, hash, chunked); rec.Code != http.StatusLengthRequired {
		t.Fatalf("chunked PUT: %d", rec.Code)
	}
	// Malformed hash → 400.
	if rec := do(http.MethodPut, "zz", bytes.NewReader(data)); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed hash PUT: %d", rec.Code)
	}
}
