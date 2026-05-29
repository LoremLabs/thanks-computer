package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// newMemberCRUDRouter wires resolveTenantMiddleware + the two new
// CRUD handlers behind a synthetic admin auth context. Reuses the
// newTestController fixture (in-memory sqlite with the actor +
// tenant + membership tables already created and a seeded default
// tenant row).
func newMemberCRUDRouter(t *testing.T) (*Controller, http.Handler) {
	t.Helper()
	c := newTestController(t, config.Config{Personalities: "admin"})
	r := mux.NewRouter()
	protected := r.PathPrefix("/").Subrouter()
	protected.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ac := &auth.Context{
				Source: "open", Capabilities: []string{"admin:all"},
			}
			next.ServeHTTP(w, req.WithContext(auth.WithContext(req.Context(), ac)))
		})
	})
	tenantR := protected.PathPrefix("/v1/tenants/{tenant}").Subrouter()
	tenantR.Use(c.resolveTenantMiddleware)
	tenantR.HandleFunc("/auth/members", c.handleListTenantMembers).Methods(http.MethodGet)
	tenantR.HandleFunc("/auth/members", c.handleGrantMember).Methods(http.MethodPost)
	tenantR.HandleFunc("/auth/members/{actorID}", c.handleRevokeMember).Methods(http.MethodDelete)
	return c, r
}

// TestGrantMemberUpsertsCaps — POST /members upserts (actor, tenant)
// memberships. A second POST with a different cap set replaces the
// prior caps, leaving exactly one row.
func TestGrantMemberUpsertsCaps(t *testing.T) {
	c, h := newMemberCRUDRouter(t)
	ctx := context.Background()

	if err := c.registry.CreateActor(ctx, registry.Actor{ActorID: "actor_alice"}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}

	post := func(caps []string) *http.Response {
		body, _ := json.Marshal(map[string]any{
			"actor_id":     "actor_alice",
			"capabilities": caps,
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/auth/members", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Result()
	}

	if resp := post([]string{"opstack:*:read"}); resp.StatusCode != http.StatusOK {
		body, _ := readBody(resp)
		t.Fatalf("first grant: status=%d body=%s", resp.StatusCode, body)
	}
	if resp := post([]string{"admin:all"}); resp.StatusCode != http.StatusOK {
		body, _ := readBody(resp)
		t.Fatalf("second grant: status=%d body=%s", resp.StatusCode, body)
	}

	// One row, latest caps.
	m, err := c.registry.LoadMembership(ctx, "actor_alice", "tnt_default")
	if err != nil {
		t.Fatalf("LoadMembership: %v", err)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != "*:*:*" {
		t.Errorf("got caps=%v, want [*:*:*] (canonical admin:all)", m.Capabilities)
	}
}

// TestGrantMemberRejectsUnknownCaps — typo in caps → 400 with the
// offending value echoed back. Row never written.
func TestGrantMemberRejectsUnknownCaps(t *testing.T) {
	c, h := newMemberCRUDRouter(t)
	if err := c.registry.CreateActor(context.Background(), registry.Actor{ActorID: "actor_a"}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"actor_id":     "actor_a",
		"capabilities": []string{"opstack:reed"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/auth/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "opstack:reed") {
		t.Errorf("expected typo echoed in body; got %s", rr.Body.String())
	}
}

// TestGrantMemberRequiresActor — granting to a non-existent actor
// returns 404 actor_not_found rather than a 500 from a foreign-key
// failure.
func TestGrantMemberRequiresActor(t *testing.T) {
	_, h := newMemberCRUDRouter(t)
	body, _ := json.Marshal(map[string]any{
		"actor_id":     "actor_ghost",
		"capabilities": []string{"opstack:*:read"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/auth/members", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "actor_not_found") {
		t.Errorf("expected actor_not_found code; got %s", rr.Body.String())
	}
}

// TestRevokeMemberSoftDeletes — DELETE /members/{actorID} sets
// revoked_at; subsequent LoadMembership returns ErrNotFound.
func TestRevokeMemberSoftDeletes(t *testing.T) {
	c, h := newMemberCRUDRouter(t)
	ctx := context.Background()
	if err := c.registry.CreateActor(ctx, registry.Actor{ActorID: "actor_bob"}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	if _, err := c.registry.CreateMembership(ctx, registry.Membership{
		ActorID:      "actor_bob",
		TenantID:     "tnt_default",
		Capabilities: []string{"admin:all"},
	}); err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/v1/tenants/default/auth/members/actor_bob", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	if _, err := c.registry.LoadMembership(ctx, "actor_bob", "tnt_default"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("after revoke: got %v, want ErrNotFound", err)
	}
}

func readBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	return buf.String(), err
}
