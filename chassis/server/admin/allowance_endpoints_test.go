package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
	"github.com/kvtools/valkeyrie"

	"github.com/loremlabs/thanks-computer/chassis/allowance"
	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	boltdb "github.com/loremlabs/thanks-computer/chassis/kv/boltstore"
)

func allowanceController(t *testing.T) *Controller {
	t.Helper()
	c := newTestController(t, config.Config{})
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{filepath.Join(t.TempDir(), "kv.db")}, &boltdb.Config{Bucket: "txco"})
	if err != nil {
		t.Fatalf("open boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c.pu.Allowances = allowance.NewStore(kvstore.New(s, 0, 0))
	return c
}

func allowanceReq(method, name, body string, caps ...string) *http.Request {
	path := "/v1/tenants/default/allowances"
	if name != "" {
		path += "/" + name
	}
	req := withTenantAdminCtx(httptest.NewRequest(method, path, bytes.NewBufferString(body)), "tnt_default")
	if caps != nil {
		ac := *auth.FromContext(req.Context())
		ac.Capabilities = caps
		req = req.WithContext(auth.WithContext(req.Context(), &ac))
	}
	if name != "" {
		req = mux.SetURLVars(req, map[string]string{"name": name})
	}
	return req
}

func TestAllowanceEndpoints(t *testing.T) {
	c := allowanceController(t)

	rec := httptest.NewRecorder()
	c.handleSetAllowance(rec, allowanceReq(http.MethodPut, "scout", `{"fuel":500,"per":"hour"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	var got allowanceRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Defined || got.Fuel != 500 || got.Per != "hour" || got.Remaining == nil || *got.Remaining != 500 || got.ResetsAt == "" {
		t.Errorf("PUT result = %+v", got)
	}

	rec = httptest.NewRecorder()
	c.handleSetAllowance(rec, allowanceReq(http.MethodPut, "bard", `{"fuel":10}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT with default per: %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	c.handleListAllowances(rec, allowanceReq(http.MethodGet, "", ""))
	var list allowanceListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || list.Count != 2 ||
		list.Allowances[0].Name != "bard" || list.Allowances[0].Per != "day" {
		t.Fatalf("GET list = %d %s", rec.Code, rec.Body)
	}

	// An undefined name reads as metered: no limit, no remaining.
	rec = httptest.NewRecorder()
	c.handleGetAllowance(rec, allowanceReq(http.MethodGet, "ghost", ""))
	got = allowanceRecord{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Defined || got.Remaining != nil {
		t.Errorf("GET undefined = %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	c.handleDeleteAllowance(rec, allowanceReq(http.MethodDelete, "scout", ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	c.handleGetAllowance(rec, allowanceReq(http.MethodGet, "scout", ""))
	got = allowanceRecord{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Defined {
		t.Errorf("deleted allowance still defined: %s", rec.Body)
	}
}

func TestAllowanceEndpointsRefuse(t *testing.T) {
	c := allowanceController(t)
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		req  *http.Request
		want int
	}{
		{"bad period", c.handleSetAllowance, allowanceReq(http.MethodPut, "scout", `{"fuel":5,"per":"week"}`), http.StatusBadRequest},
		{"no fuel", c.handleSetAllowance, allowanceReq(http.MethodPut, "scout", `{}`), http.StatusBadRequest},
		{"unknown field", c.handleSetAllowance, allowanceReq(http.MethodPut, "scout", `{"fuel":5,"credits":1}`), http.StatusBadRequest},
		{"bad name", c.handleGetAllowance, allowanceReq(http.MethodGet, "Scout", ""), http.StatusBadRequest},
		{"write with read-only kv", c.handleSetAllowance, allowanceReq(http.MethodPut, "scout", `{"fuel":5}`, "kv:*:read"), http.StatusForbidden},
		{"read without kv", c.handleListAllowances, allowanceReq(http.MethodGet, "", "", "secret:*:*"), http.StatusForbidden},
	} {
		rec := httptest.NewRecorder()
		tc.h(rec, tc.req)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body)
		}
	}

	c.pu.Allowances = nil
	rec := httptest.NewRecorder()
	c.handleListAllowances(rec, allowanceReq(http.MethodGet, "", ""))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no store: status %d, want 503", rec.Code)
	}
}
