package auth

import (
	"testing"
)

// TestEnsureDevProfile covers the create / refresh / idempotent / keep-real
// cases, and that the written meta is keyless with the local URL + tenant.
func TestEnsureDevProfile(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())

	const url = "http://localhost:8081"

	// 1. First call writes a keyless meta.
	act, path, err := EnsureDevProfile(DevProfileName, url, DefaultTenantSlug)
	if err != nil {
		t.Fatalf("first EnsureDevProfile: %v", err)
	}
	if act != DevProfileWrote {
		t.Errorf("first action = %q, want %q", act, DevProfileWrote)
	}
	m, err := LoadMeta(path)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if m.ChassisURL != url || m.DefaultTenant != DefaultTenantSlug {
		t.Errorf("meta url=%q tenant=%q, want %q / %q", m.ChassisURL, m.DefaultTenant, url, DefaultTenantSlug)
	}
	if m.ActorID != "" || m.KeyID != "" || m.KeySource != "" {
		t.Errorf("expected keyless meta, got actor=%q key=%q source=%q", m.ActorID, m.KeyID, m.KeySource)
	}

	// 2. Second identical call is a no-op (current).
	if act, _, err := EnsureDevProfile(DevProfileName, url, DefaultTenantSlug); err != nil || act != DevProfileCurrent {
		t.Errorf("second call action=%q err=%v, want %q / nil", act, err, DevProfileCurrent)
	}

	// 3. A changed admin addr rewrites the meta.
	const url2 = "http://127.0.0.1:9090"
	if act, _, err := EnsureDevProfile(DevProfileName, url2, DefaultTenantSlug); err != nil || act != DevProfileWrote {
		t.Errorf("changed-url action=%q err=%v, want %q / nil", act, err, DevProfileWrote)
	}

	// 4. A real enrolled profile of the same name is left untouched.
	if err := SaveMeta(path, Meta{
		ActorID:    "actor_real",
		KeyID:      "key_real",
		ChassisURL: url,
		KeySource:  SourceSSHAgent,
	}); err != nil {
		t.Fatalf("seed enrolled meta: %v", err)
	}
	if act, _, err := EnsureDevProfile(DevProfileName, url, DefaultTenantSlug); err != nil || act != DevProfileKept {
		t.Errorf("enrolled-profile action=%q err=%v, want %q / nil", act, err, DevProfileKept)
	}
	if m, _ := LoadMeta(path); m == nil || m.ActorID != "actor_real" {
		t.Errorf("enrolled meta was clobbered: %+v", m)
	}
}

// TestEnsureDevProfileRejectsRemote verifies a non-local chassis is refused —
// a keyless profile for a remote endpoint would silently send unsigned.
func TestEnsureDevProfileRejectsRemote(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	if _, _, err := EnsureDevProfile(DevProfileName, "https://chassis.example.com:8081", DefaultTenantSlug); err == nil {
		t.Fatal("expected error for non-local chassis, got nil")
	}
}

// TestBuildSignedTargetKeylessLocal: the keyless `dev` profile resolves to an
// UNSIGNED target (nil Auth) against a local chassis — no "no signing key"
// error. This is what makes `--target dev` work with no bootstrap-local.
func TestBuildSignedTargetKeylessLocal(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	if _, _, err := EnsureDevProfile(DevProfileName, "http://localhost:8081", DefaultTenantSlug); err != nil {
		t.Fatalf("EnsureDevProfile: %v", err)
	}
	tg, err := buildSignedTarget(DevProfileName, "")
	if err != nil {
		t.Fatalf("buildSignedTarget(dev): %v", err)
	}
	if tg.Addr != "http://localhost:8081" {
		t.Errorf("Addr = %q, want http://localhost:8081", tg.Addr)
	}
	if tg.Auth != nil {
		t.Error("expected nil Auth (unsigned) for a keyless local profile")
	}
}

// TestBuildSignedTargetKeylessRemoteErrors: a keyless profile pointing at a
// REMOTE chassis is a hard error — prod requires a real key.
func TestBuildSignedTargetKeylessRemoteErrors(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	mp, err := MetaPath("staging")
	if err != nil {
		t.Fatalf("MetaPath: %v", err)
	}
	if err := SaveMeta(mp, Meta{ChassisURL: "https://chassis.example.com:8081", DefaultTenant: "default"}); err != nil {
		t.Fatalf("SaveMeta: %v", err)
	}
	if _, err := buildSignedTarget("staging", ""); err == nil {
		t.Fatal("expected error for a keyless remote profile, got nil")
	}
}
