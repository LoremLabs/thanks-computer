package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPutAndHeadCompute(t *testing.T) {
	var putPath, putBody, putQuery string
	var headPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			putPath = r.URL.Path
			putQuery = r.URL.RawQuery
			b, _ := io.ReadAll(r.Body)
			putBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"compute://sha256/abc","bytes":3}`))
		case http.MethodHead:
			headPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	c := New(Target{Addr: srv.URL, Tenant: "acme"})
	ctx := context.Background()

	if err := c.PutCompute(ctx, "sha256", "abc", "wazero", []byte("\x00asm")); err != nil {
		t.Fatalf("PutCompute: %v", err)
	}
	if want := "/v1/tenants/acme/computes/sha256/abc"; putPath != want {
		t.Fatalf("PUT path = %q, want %q", putPath, want)
	}
	if putQuery != "engine=wazero" {
		t.Fatalf("PUT query = %q, want engine=wazero", putQuery)
	}
	if putBody != "\x00asm" {
		t.Fatalf("PUT body = %q, want the wasm bytes", putBody)
	}

	ok, err := c.HeadCompute(ctx, "sha256", "abc")
	if err != nil || !ok {
		t.Fatalf("HeadCompute = (%v, %v), want (true, nil)", ok, err)
	}
	if want := "/v1/tenants/acme/computes/sha256/abc"; headPath != want {
		t.Fatalf("HEAD path = %q, want %q", headPath, want)
	}
}

func TestHeadComputeNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := New(Target{Addr: srv.URL, Tenant: "acme"})
	ok, err := c.HeadCompute(context.Background(), "sha256", "missing")
	if err != nil {
		t.Fatalf("HeadCompute err = %v, want nil", err)
	}
	if ok {
		t.Fatal("HeadCompute = true for missing artifact, want false")
	}
}
