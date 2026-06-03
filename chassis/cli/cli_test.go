package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDispatchUnknownSubcommand verifies that a bare word that isn't a
// known subcommand fails loudly instead of falling through to the
// server-boot path — a typo like `txco aply` should be told it's
// unknown, not silently boot the chassis.
func TestDispatchUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status, ok := Dispatch([]string{"txco", "aply"}, &stdout, &stderr)
	if !ok {
		t.Fatalf("ok=false: dispatcher fell through to server boot")
	}
	if status == 0 {
		t.Fatalf("status=0: want non-zero exit for unknown subcommand")
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "aply"`) {
		t.Fatalf("stderr lacks unknown-subcommand message: %q", stderr.String())
	}
}

// TestDispatchWhoamiAlias verifies `txco whoami` routes to `auth whoami`
// (a top-level convenience alias) rather than failing as unknown. Points
// at an httptest chassis so the round-trip is deterministic and offline.
func TestDispatchWhoamiAlias(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/whoami" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"source":"open","capabilities":[]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	status, ok := Dispatch([]string{"txco", "whoami", "--url", srv.URL}, &stdout, &stderr)
	if !ok {
		t.Fatalf("ok=false: whoami alias fell through to server boot")
	}
	if strings.Contains(stderr.String(), "unknown subcommand") {
		t.Fatalf("whoami treated as unknown subcommand: %q", stderr.String())
	}
	if status != 0 {
		t.Fatalf("status=%d, want 0; stderr=%q", status, stderr.String())
	}
	// Proves the alias actually reached `auth whoami` (its output shape).
	if !strings.Contains(stdout.String(), "source: open") {
		t.Fatalf("whoami alias did not reach `auth whoami`; stdout=%q", stdout.String())
	}
}

// TestDispatchFlagsFallThrough verifies that leading flags still hand
// off to the server boot path so `txco --web-addr=:8080` works the
// same as before.
func TestDispatchFlagsFallThrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status, ok := Dispatch([]string{"txco", "--web-addr=:8080"}, &stdout, &stderr)
	if ok {
		t.Fatalf("ok=true: leading flag should fall through to server boot")
	}
	if status != 0 {
		t.Fatalf("status=%d: expected 0 on fall-through", status)
	}
}

// TestDispatchServePassthrough — explicit `serve` keeps falling
// through (main.go strips it before config.Load runs).
func TestDispatchServePassthrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, ok := Dispatch([]string{"txco", "serve"}, &stdout, &stderr)
	if ok {
		t.Fatalf("ok=true: `txco serve` should fall through to server boot")
	}
}

// TestDispatchNoArgs — bare `txco` prints help (Stripe/gcloud-style).
// Starting the server now requires explicit `txco serve`; bare `txco`
// used to fall through to server boot, which was surprising for a
// "what does this binary do?" first encounter.
func TestDispatchNoArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status, ok := Dispatch([]string{"txco"}, &stdout, &stderr)
	if !ok {
		t.Fatalf("ok=false: bare `txco` should print help and return ok=true")
	}
	if status != 0 {
		t.Fatalf("status=%d: bare `txco` help should exit 0", status)
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("stdout lacks Usage block: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "txco serve") {
		t.Fatalf("stdout should mention `txco serve` so users know how to start the server: %q", stdout.String())
	}
}
