package auth

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// slugEnrollServer returns an httptest server that asks for a tenant slug on
// the first (slug-less) request and succeeds once one is supplied. It records
// every decoded request body for assertions.
func slugEnrollServer(t *testing.T, got *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		*got = append(*got, body)
		w.Header().Set("Content-Type", "application/json")
		if s, _ := body["tenant_slug"].(string); s == "" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":  "tenant_slug_required",
				"detail": map[string]any{"suggested_tenant_slug": "matt"},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"chassis_url":  "https://chassis.test",
			"tenant_slug":  body["tenant_slug"],
			"actor_id":     "actor_test",
			"key_id":       "key_test",
			"capabilities": []string{"opstack:*:*", "stack:*:*", "hostname:*:*"},
		})
	}))
}

// TestOAuthEnrollAutoSuggestNonTTY: with no TTY and no explicit --tenant, a
// first-enroll 409 is resolved by auto-resubmitting the server's suggestion;
// the profile is written and made active.
func TestOAuthEnrollAutoSuggestNonTTY(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())

	var got []map[string]any
	srv := slugEnrollServer(t, &got)
	defer srv.Close()

	res, err := OAuthEnroll(OAuthEnrollOptions{
		EndpointURL: srv.URL,
		IDToken:     "header.payload.sig",
		Profile:     "cloud",
		Label:       "matt@macbook",
		NewKey:      true, // deterministic: skip ssh-agent / ~/.ssh
		Stderr:      io.Discard,
	})
	if err != nil {
		t.Fatalf("OAuthEnroll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 requests (slug-less then resubmit), got %d", len(got))
	}
	if s, _ := got[1]["tenant_slug"].(string); s != "matt" {
		t.Fatalf("resubmit tenant_slug = %q, want matt", s)
	}
	if res.TenantSlug != "matt" || res.ActorID != "actor_test" {
		t.Fatalf("result = %+v", res)
	}

	// Meta written with the chassis URL + default tenant from the response.
	m, err := LoadMeta(res.MetaPath)
	if err != nil {
		t.Fatalf("LoadMeta(%s): %v", res.MetaPath, err)
	}
	if m.ChassisURL != "https://chassis.test" || m.DefaultTenant != "matt" || m.ActorID != "actor_test" {
		t.Fatalf("meta = %+v", m)
	}
	// Profile made active.
	active, err := ReadActiveProfile()
	if err != nil {
		t.Fatalf("ReadActiveProfile: %v", err)
	}
	if active != res.Profile {
		t.Fatalf("active profile = %q, want %q", active, res.Profile)
	}
}

// TestOAuthEnrollExplicitTenantConflict: an explicit --tenant that the server
// rejects is a hard error — never silently swapped for a suggestion.
func TestOAuthEnrollExplicitTenantConflict(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  "tenant_slug_taken",
			"detail": map[string]any{"suggested_tenant_slug": "taken-2"},
		})
	}))
	defer srv.Close()

	_, err := OAuthEnroll(OAuthEnrollOptions{
		EndpointURL: srv.URL,
		IDToken:     "header.payload.sig",
		Profile:     "cloud",
		TenantSlug:  "taken",
		NewKey:      true,
		Stderr:      io.Discard,
	})
	if err == nil {
		t.Fatalf("expected a hard error for an explicit --tenant conflict")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Fatalf("error = %v, want a 'not available' tenant-slug error", err)
	}
}
