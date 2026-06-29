package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// TestDispatchUseAlias: `txco use <profile>` routes to `auth profile use` and
// actually switches the active profile.
func TestDispatchUseAlias(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	mp, err := auth.MetaPath("dev")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SaveMeta(mp, auth.Meta{ChassisURL: "http://localhost:8081", DefaultTenant: "default"}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	status, ok := Dispatch([]string{"txco", "use", "dev"}, &out, &errb)
	if !ok || status != 0 {
		t.Fatalf("use dev: status=%d ok=%v stderr=%s", status, ok, errb.String())
	}
	if !strings.Contains(out.String(), "active profile: dev") {
		t.Fatalf("missing confirmation: %q", out.String())
	}
	if active, _ := auth.ReadActiveProfile(); active != "dev" {
		t.Fatalf("active profile = %q, want dev", active)
	}
}

// TestDispatchUseUnknownProfile: routing reaches runProfileUse (reports "not
// found", exit 1) rather than the unknown-subcommand path (exit 2) — proves
// `use` is dispatched as an alias, not treated as an unknown verb.
func TestDispatchUseUnknownProfile(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	var out, errb bytes.Buffer
	status, ok := Dispatch([]string{"txco", "use", "nope-xyz"}, &out, &errb)
	if !ok || status != 1 {
		t.Fatalf("use nope: status=%d ok=%v (want 1); stderr=%s", status, ok, errb.String())
	}
	if !strings.Contains(errb.String(), "not found") {
		t.Fatalf("expected not-found message, got %q", errb.String())
	}
}
