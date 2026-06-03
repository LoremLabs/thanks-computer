package update

import "testing"

func TestResolve(t *testing.T) {
	// Hermetic: a dev machine with HOMEBREW_* set must not perturb the
	// path-based cases below.
	t.Setenv("HOMEBREW_PREFIX", "")
	t.Setenv("HOMEBREW_CELLAR", "")

	cases := []struct {
		name     string
		origin   string
		exePath  string
		wantName string
		wantSelf bool
	}{
		{"release in arm64 cellar", "release", "/opt/homebrew/Cellar/txco/0.2.3/bin/txco", "homebrew", false},
		{"release in intel cellar", "release", "/usr/local/Cellar/txco/0.2.3/bin/txco", "homebrew", false},
		{"release self-managed /usr/local/bin", "release", "/usr/local/bin/txco", "manual", true},
		{"release self-managed home dir", "release", "/Users/x/bin/txco", "manual", true},
		{"source anywhere", "source", "/opt/homebrew/Cellar/txco/0/bin/txco", "source", false},
		{"empty origin", "", "/anywhere/txco", "source", false},
		{"unknown origin (future apt/nix)", "apt", "/usr/bin/txco", "unknown", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Resolve(c.origin, c.exePath)
			if m.Name != c.wantName || m.SelfUpdate != c.wantSelf {
				t.Errorf("Resolve(%q, %q) = {%s, %v}, want {%s, %v}",
					c.origin, c.exePath, m.Name, m.SelfUpdate, c.wantName, c.wantSelf)
			}
		})
	}
}

func TestResolveHomebrewEnv(t *testing.T) {
	// A non-Cellar path is still recognized as Homebrew via $HOMEBREW_PREFIX.
	t.Setenv("HOMEBREW_CELLAR", "")
	t.Setenv("HOMEBREW_PREFIX", "/custom/brew")
	if m := Resolve("release", "/custom/brew/bin/txco"); m.Name != "homebrew" || m.SelfUpdate {
		t.Errorf("Resolve under $HOMEBREW_PREFIX = {%s,%v}, want {homebrew,false}", m.Name, m.SelfUpdate)
	}
}

func TestUpgradeGuidance(t *testing.T) {
	if g := UpgradeGuidance(Method{Name: "manual", SelfUpdate: true}); g != "" {
		t.Errorf("manual guidance = %q, want empty (caller self-updates)", g)
	}
	for _, name := range []string{"homebrew", "source", "unknown"} {
		if g := UpgradeGuidance(Method{Name: name}); g == "" {
			t.Errorf("%s guidance is empty, want instructions", name)
		}
	}
}
