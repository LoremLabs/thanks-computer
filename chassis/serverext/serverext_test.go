package serverext

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

// TestRegistriesEmptyByDefault documents the safe default: open core registers
// no mounters, so a self-hosted chassis mounts nothing extra. (No overlay is
// imported into this package's test binary.)
func TestRegistriesEmptyByDefault(t *testing.T) {
	if got := len(PublicMounters()); got != 0 {
		t.Fatalf("PublicMounters: want 0 by default, got %d", got)
	}
	if got := len(TenantMounters()); got != 0 {
		t.Fatalf("TenantMounters: want 0 by default, got %d", got)
	}
}

// TestMountersServe registers a public and a tenant-scoped mounter, applies them
// the way admin.Start() does (public on the raw mux before the catch-all; tenant
// on a /v1/tenants/{tenant} subrouter), and asserts both routes serve.
func TestMountersServe(t *testing.T) {
	RegisterPublic(func(r *mux.Router) {
		r.HandleFunc("/ext/webhook", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}).Methods(http.MethodPost)
	})
	RegisterTenant(func(r *mux.Router) {
		r.HandleFunc("/ext/thing", func(w http.ResponseWriter, req *http.Request) {
			// The {tenant} var is resolvable from the parent prefix.
			if mux.Vars(req)["tenant"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}).Methods(http.MethodGet)
	})

	r := mux.NewRouter()
	for _, m := range PublicMounters() {
		m(r)
	}
	tenantR := r.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	for _, m := range TenantMounters() {
		m(tenantR)
	}

	srv := httptest.NewServer(r)
	defer srv.Close()

	if resp, err := http.Post(srv.URL+"/ext/webhook", "application/json", nil); err != nil {
		t.Fatalf("public route POST: %v", err)
	} else if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("public route: want 202, got %d", resp.StatusCode)
	}

	if resp, err := http.Get(srv.URL + "/v1/tenants/acme/ext/thing"); err != nil {
		t.Fatalf("tenant route GET: %v", err)
	} else if resp.StatusCode != http.StatusOK {
		t.Fatalf("tenant route: want 200, got %d", resp.StatusCode)
	}
}
