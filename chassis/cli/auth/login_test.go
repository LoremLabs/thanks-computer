package auth

import (
	"bytes"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// TestLoginIdentitySummary pins the "who are we opening the admin UI as"
// line shown by `txco ui` / `txco auth login`: label preferred, then
// actor_id, then a profile-only fallback when no meta exists. Always
// carries the tenant + chassis so the user can confirm the target.
func TestLoginIdentitySummary(t *testing.T) {
	withHome(t)
	target := client.Target{Addr: "http://localhost:8081", Tenant: "default"}

	t.Run("label preferred", func(t *testing.T) {
		seedMeta(t, "withlabel", Meta{Label: "matt@semicolons.com", ActorID: "actor_123"})
		got := loginIdentitySummary("withlabel", target)
		for _, want := range []string{"matt@semicolons.com", `profile "withlabel"`, `tenant "default"`, "http://localhost:8081"} {
			if !strings.Contains(got, want) {
				t.Errorf("summary %q missing %q", got, want)
			}
		}
		if strings.Contains(got, "actor_123") {
			t.Errorf("label should win over actor_id; got %q", got)
		}
	})

	t.Run("actor_id when no label", func(t *testing.T) {
		seedMeta(t, "noLabel", Meta{ActorID: "actor_456"})
		got := loginIdentitySummary("noLabel", target)
		if !strings.Contains(got, "actor_456") {
			t.Errorf("summary %q should fall back to actor_id", got)
		}
	})

	t.Run("profile-only fallback when no meta", func(t *testing.T) {
		got := loginIdentitySummary("ghost", target)
		if !strings.Contains(got, `profile "ghost"`) {
			t.Errorf("summary %q should name the profile when no meta exists", got)
		}
		if !strings.Contains(got, "http://localhost:8081") {
			t.Errorf("summary %q should still carry the chassis URL", got)
		}
	})
}

// TestLoginKeylessLocalOpensAdminUI: against a keyless local profile (the `dev`
// profile `txco dev` registers), `txco ui` / `auth login` skips the signed
// browser-session bootstrap — which needs an enrolled actor — and just points
// at /admin/ on the open chassis. --no-open keeps it network-free.
func TestLoginKeylessLocalOpensAdminUI(t *testing.T) {
	withHome(t)
	if _, _, err := EnsureDevProfile(DevProfileName, "http://localhost:8081", DefaultTenantSlug); err != nil {
		t.Fatalf("EnsureDevProfile: %v", err)
	}

	// Case 1: dev profile active, no positional — `txco ui`.
	if err := WriteActiveProfile(DevProfileName); err != nil {
		t.Fatalf("WriteActiveProfile: %v", err)
	}
	var out, errb bytes.Buffer
	if code := runLogin([]string{"--no-open"}, &out, &errb); code != 0 {
		t.Fatalf("runLogin(--no-open) = %d, stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "http://localhost:8081/admin/") {
		t.Errorf("expected /admin/ URL, got stdout=%q stderr=%q", out.String(), errb.String())
	}

	// Case 2: dev selected via positional — `txco ui dev` — regardless of active.
	if err := WriteActiveProfile("none"); err != nil {
		t.Fatalf("WriteActiveProfile(none): %v", err)
	}
	out.Reset()
	errb.Reset()
	if code := runLogin([]string{"--no-open", DevProfileName}, &out, &errb); code != 0 {
		t.Fatalf("runLogin(--no-open dev) = %d, stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "http://localhost:8081/admin/") {
		t.Errorf("positional: expected /admin/ URL, got stdout=%q stderr=%q", out.String(), errb.String())
	}
}

// TestLoginKeylessRemoteErrors: no key against a REMOTE chassis still demands an
// identity — a browser session can't be minted unsigned.
func TestLoginKeylessRemoteErrors(t *testing.T) {
	withHome(t)
	if err := WriteActiveProfile("none"); err != nil {
		t.Fatalf("WriteActiveProfile(none): %v", err)
	}
	var out, errb bytes.Buffer
	if code := runLogin([]string{"--no-open", "--url", "https://admin.example.com:8081"}, &out, &errb); code == 0 {
		t.Fatalf("expected non-zero exit for keyless remote, got 0; stdout=%q", out.String())
	}
}
