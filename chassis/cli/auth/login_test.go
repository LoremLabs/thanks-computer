package auth

import (
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
