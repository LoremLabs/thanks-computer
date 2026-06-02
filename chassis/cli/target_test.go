package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// TestResolveTargetPrecedence verifies the layering: flags beat env, env
// beats the on-disk config file, file beats the hardcoded default.
func TestResolveTargetPrecedence(t *testing.T) {
	clearEnv := func() {
		t.Helper()
		for _, k := range []string{"TXCO_ADMIN_ADDR", "TXCO_ADMIN_USER", "TXCO_ADMIN_PASS"} {
			t.Setenv(k, "")
		}
	}

	t.Run("default when nothing configured", func(t *testing.T) {
		clearEnv()
		// Isolate $TXCO_HOME so the address fallback (which now follows the
		// active profile) finds none and stays at the localhost default.
		t.Setenv("TXCO_HOME", t.TempDir())
		t.Setenv("TXCO_PROFILE", "")
		got := resolveTarget(t.TempDir(), "", "", "", "", "")
		if got.Addr != "http://localhost:8081" || got.User != "" || got.Pass != "" {
			t.Errorf("got %+v, want default localhost target", got)
		}
	})

	t.Run("legacy flat txco.yaml (addr/user/pass)", func(t *testing.T) {
		clearEnv()
		dir := t.TempDir()
		body := "addr: https://chassis.example.com:8081\nuser: alice\npass: s3cret\n"
		if err := os.WriteFile(filepath.Join(dir, "txco.yaml"), []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := resolveTarget(dir, "", "", "", "", "")
		if got.Addr != "https://chassis.example.com:8081" || got.User != "alice" || got.Pass != "s3cret" {
			t.Errorf("got %+v, want yaml-loaded values", got)
		}
	})

	t.Run("targets block — default target", func(t *testing.T) {
		clearEnv()
		dir := t.TempDir()
		body := `target: dev
targets:
  dev:
    chassis: http://dev.example.com
    user: devuser
  prod:
    chassis: https://prod.example.com
    user: produser
    pass: prodpass
`
		_ = os.WriteFile(filepath.Join(dir, "txco.yaml"), []byte(body), 0o644)
		got := resolveTarget(dir, "", "", "", "", "")
		if got.Addr != "http://dev.example.com" || got.User != "devuser" {
			t.Errorf("got %+v, want dev target picked up by default", got)
		}
	})

	t.Run("explicit --target overrides default", func(t *testing.T) {
		clearEnv()
		dir := t.TempDir()
		body := `target: dev
targets:
  dev:
    chassis: http://dev
  prod:
    chassis: https://prod
    user: bob
    pass: ssh
`
		_ = os.WriteFile(filepath.Join(dir, "txco.yaml"), []byte(body), 0o644)
		got := resolveTarget(dir, "prod", "", "", "", "")
		if got.Addr != "https://prod" || got.User != "bob" || got.Pass != "ssh" {
			t.Errorf("got %+v, want prod target", got)
		}
	})

	t.Run("legacy .txco/target.yaml still works", func(t *testing.T) {
		clearEnv()
		dir := t.TempDir()
		legacy := filepath.Join(dir, ".txco")
		_ = os.MkdirAll(legacy, 0o755)
		_ = os.WriteFile(filepath.Join(legacy, "target.yaml"), []byte("addr: http://from-legacy\n"), 0o644)
		got := resolveTarget(dir, "", "", "", "", "")
		if got.Addr != "http://from-legacy" {
			t.Errorf("got addr %q, want http://from-legacy from legacy path", got.Addr)
		}
	})

	t.Run("txco.yaml beats legacy .txco/target.yaml", func(t *testing.T) {
		clearEnv()
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "txco.yaml"), []byte("addr: http://new\n"), 0o644)
		legacy := filepath.Join(dir, ".txco")
		_ = os.MkdirAll(legacy, 0o755)
		_ = os.WriteFile(filepath.Join(legacy, "target.yaml"), []byte("addr: http://legacy\n"), 0o644)
		got := resolveTarget(dir, "", "", "", "", "")
		if got.Addr != "http://new" {
			t.Errorf("got addr %q, want http://new (top-level wins over legacy)", got.Addr)
		}
	})

	t.Run("env beats file", func(t *testing.T) {
		clearEnv()
		dir := t.TempDir()
		_ = os.WriteFile(filepath.Join(dir, "txco.yaml"), []byte("addr: http://from-file\n"), 0o644)
		t.Setenv("TXCO_ADMIN_ADDR", "http://from-env")
		got := resolveTarget(dir, "", "", "", "", "")
		if got.Addr != "http://from-env" {
			t.Errorf("got addr %q, want env value", got.Addr)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		clearEnv()
		t.Setenv("TXCO_ADMIN_ADDR", "http://from-env")
		got := resolveTarget(t.TempDir(), "", "http://from-flag", "", "", "")
		if got.Addr != "http://from-flag" {
			t.Errorf("got addr %q, want flag value", got.Addr)
		}
	})

	// A profile's bound chassis_url fills the otherwise-blind localhost
	// default — for an explicit --profile AND (now) the active profile,
	// mirroring ResolveTenant. A workspace target / --addr still win.
	t.Run("profile chassis_url beats localhost default", func(t *testing.T) {
		clearEnv()
		home := t.TempDir()
		t.Setenv("TXCO_HOME", home)
		t.Setenv("TXCO_PROFILE", "")
		mp, err := auth.MetaPath("prodprof")
		if err != nil {
			t.Fatalf("MetaPath: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(mp), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(mp,
			[]byte(`{"actor_id":"a","key_id":"k","chassis_url":"https://admin.example"}`), 0o644); err != nil {
			t.Fatalf("write meta: %v", err)
		}

		// Explicit profile, no --addr, no txco.yaml → profile's URL.
		got := resolveTarget(t.TempDir(), "", "", "", "", "prodprof")
		if got.Addr != "https://admin.example" {
			t.Errorf("explicit profile: got addr %q, want https://admin.example", got.Addr)
		}

		// Bare call with NO active profile → localhost (nothing to follow).
		if got := resolveTarget(t.TempDir(), "", "", "", "", ""); got.Addr != "http://localhost:8081" {
			t.Errorf("bare, no active profile: got addr %q, want localhost", got.Addr)
		}

		// Once prodprof is ACTIVE, a bare call follows its chassis too
		// (the asymmetry fix: addr now follows the active profile like the
		// tenant already does).
		if err := auth.WriteActiveProfile("prodprof"); err != nil {
			t.Fatalf("WriteActiveProfile: %v", err)
		}
		if got := resolveTarget(t.TempDir(), "", "", "", "", ""); got.Addr != "https://admin.example" {
			t.Errorf("bare, active profile: got addr %q, want https://admin.example", got.Addr)
		}

		// Explicit --addr still wins over the profile URL.
		if got := resolveTarget(t.TempDir(), "", "http://flag", "", "", "prodprof"); got.Addr != "http://flag" {
			t.Errorf("flag vs profile: got addr %q, want http://flag", got.Addr)
		}

		// A workspace txco.yaml target still wins over the active profile.
		wdir := t.TempDir()
		_ = os.WriteFile(filepath.Join(wdir, "txco.yaml"), []byte("addr: http://workspace\n"), 0o644)
		if got := resolveTarget(wdir, "", "", "", "", ""); got.Addr != "http://workspace" {
			t.Errorf("workspace target vs active profile: got addr %q, want http://workspace", got.Addr)
		}
	})
}

// TestResolveFullTargetMergesOps covers the operations-map merge: target
// overrides win over top-level defaults; ops only in the default carry
// through; ops only in the override are added.
func TestResolveFullTargetMergesOps(t *testing.T) {
	cfg := &workspaceConfig{
		DefaultTarget: "dev",
		Operations: map[string]operationConfig{
			"CLASSIFY": {URL: "http://localhost:4101/op"},
			"NOTIFY":   {URL: "http://localhost:4102/op"},
		},
		Targets: map[string]targetConfig{
			"dev": {
				Chassis: "http://localhost:8081",
				Mock:    "allow",
			},
			"prod": {
				Chassis: "https://prod.example.com:8081",
				User:    "alice",
				Mock:    "deny",
				Operations: map[string]operationConfig{
					"CLASSIFY": {URL: "https://classify.example.com/op"},
					// NOTIFY not overridden — should fall through to default
					"AUDIT": {URL: "https://audit.example.com/op"}, // prod-only
				},
			},
		},
	}

	t.Run("dev target — no overrides, uses defaults", func(t *testing.T) {
		got := resolveFullTargetFromConfig(cfg, "dev")
		if got.Name != "dev" {
			t.Errorf("Name = %q, want dev", got.Name)
		}
		if got.Chassis != "http://localhost:8081" {
			t.Errorf("Chassis = %q", got.Chassis)
		}
		if got.Mock != "allow" {
			t.Errorf("Mock = %q, want allow", got.Mock)
		}
		if got.Operations["CLASSIFY"].URL != "http://localhost:4101/op" {
			t.Errorf("CLASSIFY = %q, want default", got.Operations["CLASSIFY"].URL)
		}
		if got.Operations["NOTIFY"].URL != "http://localhost:4102/op" {
			t.Errorf("NOTIFY = %q, want default", got.Operations["NOTIFY"].URL)
		}
		if _, ok := got.Operations["AUDIT"]; ok {
			t.Errorf("AUDIT should NOT exist in dev (only declared in prod overrides)")
		}
	})

	t.Run("prod target — overrides override, defaults fall through", func(t *testing.T) {
		got := resolveFullTargetFromConfig(cfg, "prod")
		if got.Chassis != "https://prod.example.com:8081" {
			t.Errorf("Chassis = %q", got.Chassis)
		}
		if got.User != "alice" {
			t.Errorf("User = %q, want alice", got.User)
		}
		if got.Mock != "deny" {
			t.Errorf("Mock = %q, want deny", got.Mock)
		}
		if got.Operations["CLASSIFY"].URL != "https://classify.example.com/op" {
			t.Errorf("CLASSIFY URL = %q, want prod override", got.Operations["CLASSIFY"].URL)
		}
		if got.Operations["NOTIFY"].URL != "http://localhost:4102/op" {
			t.Errorf("NOTIFY URL = %q, want default fall-through", got.Operations["NOTIFY"].URL)
		}
		if got.Operations["AUDIT"].URL != "https://audit.example.com/op" {
			t.Errorf("AUDIT URL = %q, want prod-only addition", got.Operations["AUDIT"].URL)
		}
	})

	t.Run("default target picks `target:` field when --target empty", func(t *testing.T) {
		got := resolveFullTargetFromConfig(cfg, "")
		if got.Name != "dev" {
			t.Errorf("default Name = %q, want dev (from `target:` field)", got.Name)
		}
	})
}

// TestResolveFullTargetLegacyFlatConfig verifies that a config carrying
// only addr/user/pass (no targets block) synthesizes a usable target.
func TestResolveFullTargetLegacyFlatConfig(t *testing.T) {
	cfg := &workspaceConfig{
		Addr: "https://legacy.example.com",
		User: "bob",
		Pass: "secret",
	}
	got := resolveFullTargetFromConfig(cfg, "")
	if got.Chassis != "https://legacy.example.com" {
		t.Errorf("Chassis = %q, want legacy addr", got.Chassis)
	}
	if got.User != "bob" || got.Pass != "secret" {
		t.Errorf("got user=%q pass=%q, want bob/secret", got.User, got.Pass)
	}
	if got.Mock != "allow" {
		t.Errorf("Mock = %q, want allow (default for legacy)", got.Mock)
	}
}

// TestResolveFullTargetMissingNamedTarget falls through to localhost
// defaults; apply/dev surface a clearer error before this point but the
// resolver itself stays total.
func TestResolveFullTargetMissingNamedTarget(t *testing.T) {
	cfg := &workspaceConfig{
		Targets: map[string]targetConfig{
			"dev": {Chassis: "http://localhost:8081"},
		},
	}
	got := resolveFullTargetFromConfig(cfg, "nonexistent")
	if got.Chassis != "http://localhost:8081" {
		t.Errorf("missing target should fall back to localhost default, got %q", got.Chassis)
	}
}
