package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSetStackHostMint verifies the PATCH /settings request shape: method,
// path, JSON body, and that the response is decoded back.
func TestSetStackHostMint(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"mint_hostname":false}`))
	}))
	t.Cleanup(srv.Close)

	c := New(Target{Addr: srv.URL, Tenant: "acme"})
	res, err := c.SetStackHostMint(context.Background(), "publications/white-fang", false, false)
	if err != nil {
		t.Fatalf("SetStackHostMint: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", gotMethod)
	}
	if want := "/v1/tenants/acme/stacks/publications/white-fang/settings"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	if !strings.Contains(gotBody, `"mint_hostname":false`) {
		t.Fatalf("body = %q, want it to contain \"mint_hostname\":false", gotBody)
	}
	if res.MintHostname {
		t.Fatalf("response MintHostname = true, want false")
	}
}
