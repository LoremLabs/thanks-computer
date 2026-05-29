package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestDispatchUnknownSubcommand verifies that a bare word that isn't a
// known subcommand fails loudly instead of falling through to the
// server-boot path. `txco whoami` used to silently start the chassis;
// the intent of the user was almost always `txco auth whoami`.
func TestDispatchUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	status, ok := Dispatch([]string{"txco", "whoami"}, &stdout, &stderr)
	if !ok {
		t.Fatalf("ok=false: dispatcher fell through to server boot")
	}
	if status == 0 {
		t.Fatalf("status=0: want non-zero exit for unknown subcommand")
	}
	if !strings.Contains(stderr.String(), `unknown subcommand "whoami"`) {
		t.Fatalf("stderr lacks unknown-subcommand message: %q", stderr.String())
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
