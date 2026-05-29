package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestScopedURLWithTenant — when Target.Tenant is set, tenant-scoped
// endpoints get the /v1/tenants/<slug>/ prefix and chassis-wide ones
// don't. Verifies the phase-2 URL contract that the server-side
// resolveTenantMiddleware expects.
func TestScopedURLWithTenant(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/ops"):
			_, _ = w.Write([]byte(`{"ops":[]}`))
		case strings.HasSuffix(r.URL.Path, "/auth/invitations"):
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"invitations":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"invitation_id":"inv_x","token":"t","expires_at":"2026-01-01T00:00:00Z"}`))
			}
		case strings.HasSuffix(r.URL.Path, "/auth/whoami"):
			_, _ = w.Write([]byte(`{"source":"open","capabilities":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := New(Target{Addr: srv.URL, Tenant: "loremlabs"})
	ctx := context.Background()

	if _, err := c.ListOps(ctx, ""); err != nil {
		t.Fatalf("ListOps: %v", err)
	}
	if _, err := c.CreateInvitation(ctx, CreateInvitationRequest{Label: "a"}); err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	if _, err := c.ListInvitations(ctx); err != nil {
		t.Fatalf("ListInvitations: %v", err)
	}
	if _, err := c.Whoami(ctx); err != nil {
		t.Fatalf("Whoami: %v", err)
	}

	want := []string{
		"/v1/tenants/loremlabs/ops",
		"/v1/tenants/loremlabs/auth/invitations",
		"/v1/tenants/loremlabs/auth/invitations",
		"/auth/whoami", // chassis-wide; NOT tenant-prefixed
	}
	if len(gotPaths) != len(want) {
		t.Fatalf("got paths %v; want %v", gotPaths, want)
	}
	for i, w := range want {
		if gotPaths[i] != w {
			t.Errorf("path[%d]: got %q, want %q", i, gotPaths[i], w)
		}
	}
}

// TestScopedURLDefaultsTenant — phase 7 retired the legacy flat
// routes; a Target with no Tenant now falls through to the literal
// "default" tenant slug (the bottom rung of ResolveTenant). This
// keeps tests that construct Target{} directly from accidentally
// hitting the 410 path.
func TestScopedURLDefaultsTenant(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ops":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := New(Target{Addr: srv.URL}) // no tenant
	if _, err := c.ListOps(context.Background(), ""); err != nil {
		t.Fatalf("ListOps: %v", err)
	}
	if gotPath != "/v1/tenants/default/ops" {
		t.Errorf("expected /v1/tenants/default/ops; got %q", gotPath)
	}
}
