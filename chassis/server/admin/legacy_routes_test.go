package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
)

// TestRouteRetired410 — every retired flat path returns 410 with the
// route_retired error code AND a detail.new URL hint pointing at the
// tenant-scoped replacement. We register the handler on a thin
// router (no auth middleware) so the test isolates the 410 behavior
// from auth-flow concerns.
func TestRouteRetired410(t *testing.T) {
	c := &Controller{}
	r := mux.NewRouter()
	r.HandleFunc("/v1/ops", c.handleRouteRetired).Methods(http.MethodGet)
	r.HandleFunc("/v1/ops/import", c.handleRouteRetired).Methods(http.MethodPost)
	r.HandleFunc("/auth/actors", c.handleRouteRetired).Methods(http.MethodGet)
	r.HandleFunc("/auth/actors/{actorID}/revoke", c.handleRouteRetired).Methods(http.MethodPost)
	r.HandleFunc("/auth/invitations", c.handleRouteRetired).Methods(http.MethodGet, http.MethodPost)
	r.HandleFunc("/auth/invitations/{invID}/revoke", c.handleRouteRetired).Methods(http.MethodPost)

	cases := []struct {
		method, path string
		newSuffix    string
	}{
		{http.MethodGet, "/v1/ops", "/ops"},
		{http.MethodPost, "/v1/ops/import", "/ops/import"},
		{http.MethodGet, "/auth/actors", "/auth/actors"},
		{http.MethodPost, "/auth/actors/actor_x/revoke", "/auth/actors/actor_x/revoke"},
		{http.MethodGet, "/auth/invitations", "/auth/invitations"},
		{http.MethodPost, "/auth/invitations", "/auth/invitations"},
		{http.MethodPost, "/auth/invitations/inv_x/revoke", "/auth/invitations/inv_x/revoke"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		rr := httptest.NewRecorder()
		r.ServeHTTP(rr, req)

		if rr.Code != http.StatusGone {
			t.Errorf("%s %s: status=%d, want 410; body=%s", tc.method, tc.path, rr.Code, rr.Body.String())
			continue
		}
		var got struct {
			Error  string         `json:"error"`
			Detail map[string]any `json:"detail"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Errorf("%s %s: decode body: %v (body=%s)", tc.method, tc.path, err, rr.Body.String())
			continue
		}
		if got.Error != "route_retired" {
			t.Errorf("%s %s: error=%q, want route_retired", tc.method, tc.path, got.Error)
		}
		want := "/v1/tenants/<tenant>" + tc.newSuffix
		if got.Detail == nil || got.Detail["new"] != want {
			t.Errorf("%s %s: detail.new=%v, want %q", tc.method, tc.path, got.Detail["new"], want)
		}
		// The hint should point the caller at the tenant-scoped shape
		// so a curl-probing operator sees what to do.
		hint, _ := got.Detail["hint"].(string)
		if !strings.Contains(hint, "/v1/tenants/") {
			t.Errorf("%s %s: hint should reference /v1/tenants/; got %q", tc.method, tc.path, hint)
		}
	}
}
