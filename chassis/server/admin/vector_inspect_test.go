package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/vector"
	"github.com/loremlabs/thanks-computer/chassis/vector/sqlitevec"
)

func TestVectorInspectHandlers(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	vs, err := sqlitevec.New(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatalf("sqlitevec: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })
	c.SetVectorStore(vs)

	// Seed the store directly (the store-seed path is tested elsewhere).
	if err := vs.EnsureCollection(ctx, "tnt_default", vector.Collection{
		Name: "books", EmbeddingModel: "text-embedding-3-small", Dimensions: 3, Metric: vector.MetricCosine,
	}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := vs.Upsert(ctx, "tnt_default", "books", []vector.Item{
		{ID: "a", Vector: []float32{1, 0, 0}}, {ID: "b", Vector: []float32{0, 1, 0}},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// ls
	rec := httptest.NewRecorder()
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/vectors", nil), "tnt_default")
	c.handleListVectorCollections(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ls status=%d body=%s", rec.Code, rec.Body.String())
	}
	var ls struct {
		Collections []vectorCollectionView `json:"collections"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ls); err != nil {
		t.Fatalf("ls decode: %v", err)
	}
	if len(ls.Collections) != 1 || ls.Collections[0].Name != "books" || ls.Collections[0].Count != 2 ||
		ls.Collections[0].Dimensions != 3 {
		t.Fatalf("ls = %+v, want one books/count=2/dims=3", ls.Collections)
	}

	// show
	rec = httptest.NewRecorder()
	req = mux.SetURLVars(
		withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/vectors/books", nil), "tnt_default"),
		map[string]string{"name": "books"})
	c.handleGetVectorCollection(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("show status=%d body=%s", rec.Code, rec.Body.String())
	}
	var show struct {
		Name  string   `json:"name"`
		Count int      `json:"count"`
		IDs   []string `json:"ids"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &show); err != nil {
		t.Fatalf("show decode: %v", err)
	}
	if show.Name != "books" || show.Count != 2 || len(show.IDs) != 2 {
		t.Fatalf("show = %+v, want books/2/[a b]", show)
	}

	// show missing → 404
	rec = httptest.NewRecorder()
	req = mux.SetURLVars(
		withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/vectors/nope", nil), "tnt_default"),
		map[string]string{"name": "nope"})
	c.handleGetVectorCollection(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("show missing status=%d, want 404", rec.Code)
	}

	// drop
	rec = httptest.NewRecorder()
	req = mux.SetURLVars(
		withTenantAdminCtx(httptest.NewRequest(http.MethodDelete, "/v1/tenants/default/vectors/books", nil), "tnt_default"),
		map[string]string{"name": "books"})
	c.handleDropVectorCollection(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("drop status=%d body=%s", rec.Code, rec.Body.String())
	}
	var drop struct {
		RemovedItems int `json:"removed_items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &drop); err != nil {
		t.Fatalf("drop decode: %v", err)
	}
	if drop.RemovedItems != 2 {
		t.Fatalf("drop removed=%d, want 2", drop.RemovedItems)
	}
	if cols, _ := vs.ListCollections(ctx, "tnt_default"); len(cols) != 0 {
		t.Fatalf("collection still present after drop: %v", cols)
	}
}

// Store unset → endpoints report disabled, never panic.
func TestVectorInspectStoreDisabled(t *testing.T) {
	c := newTestController(t, config.Config{}) // no SetVectorStore
	rec := httptest.NewRecorder()
	req := withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/vectors", nil), "tnt_default")
	c.handleListVectorCollections(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled ls status=%d, want 503", rec.Code)
	}
}
