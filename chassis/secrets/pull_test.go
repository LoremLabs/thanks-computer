package secrets

import (
	"context"
	"errors"
	"testing"
)

func TestPullPolicyAdmits(t *testing.T) {
	for _, tc := range []struct {
		policy     PullPolicy
		reviewed   bool
		unreviewed bool
	}{
		{PullNone, false, false},
		{PullReviewed, true, false},
		{PullAny, true, true},
		// Not a policy: a row from a later release, or edited by hand.
		{"", false, false},
		{"all", false, false},
		{"ANY", false, false},
	} {
		if got := tc.policy.Admits(true); got != tc.reviewed {
			t.Errorf("%q admits a reviewed node = %v, want %v", tc.policy, got, tc.reviewed)
		}
		if got := tc.policy.Admits(false); got != tc.unreviewed {
			t.Errorf("%q admits an unreviewed node = %v, want %v", tc.policy, got, tc.unreviewed)
		}
	}
	for _, ok := range []string{"none", "reviewed", "any"} {
		if p, err := ParsePullPolicy(ok); err != nil || string(p) != ok {
			t.Errorf("parse %q: %q err=%v", ok, p, err)
		}
	}
	for _, bad := range []string{"", "Any", "all", "reviewed ", "true"} {
		if p, err := ParsePullPolicy(bad); !errors.Is(err, ErrInvalidPull) {
			t.Errorf("parse %q: %q err=%v", bad, p, err)
		}
	}
}

func TestPullDefaultsToNoneAndSurvivesEveryOtherWrite(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	meta, err := s.CreateSecret(ctx, "tnt_x", nil, "DB_DSN", "the database", "actor_a", []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	// CreateSecret returns what it wrote; the row is what counts.
	if got, _ := s.LookupSecretMetadata(ctx, "tnt_x", nil, "DB_DSN"); got.Pull != PullNone {
		t.Fatalf("a new secret's pull = %q, want %q (created as %+v)", got.Pull, PullNone, meta)
	}
	list, err := s.ListSecrets(ctx, "tnt_x")
	if err != nil || len(list) != 1 || list[0].Pull != PullNone {
		t.Fatalf("list: %+v err=%v", list, err)
	}

	got, err := s.UpdateSecretPull(ctx, "tnt_x", nil, "DB_DSN", PullReviewed)
	if err != nil || got.Pull != PullReviewed {
		t.Fatalf("set reviewed: %+v err=%v", got, err)
	}
	// Nothing else moved.
	if got.Description != "the database" || got.VersionNo != 1 || got.SecretID != meta.SecretID {
		t.Errorf("setting the policy changed the secret: %+v", got)
	}
	if v, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "DB_DSN"); err != nil || string(v) != "v1" {
		t.Errorf("value after setting the policy: %q err=%v", v, err)
	}

	// A rotation and a new description leave the policy alone.
	if got, err := s.RotateSecret(ctx, "tnt_x", nil, "DB_DSN", []byte("v2")); err != nil || got.Pull != PullReviewed || got.VersionNo != 2 {
		t.Errorf("after rotate: %+v err=%v", got, err)
	}
	if got, err := s.UpdateSecretDescription(ctx, "tnt_x", nil, "DB_DSN", "renamed"); err != nil || got.Pull != PullReviewed {
		t.Errorf("after describe: %+v err=%v", got, err)
	}
	if list, _ := s.ListSecrets(ctx, "tnt_x"); len(list) != 1 || list[0].Pull != PullReviewed {
		t.Errorf("list after rotate and describe: %+v", list)
	}

	for _, p := range []PullPolicy{PullAny, PullNone} {
		if got, err := s.UpdateSecretPull(ctx, "tnt_x", nil, "DB_DSN", p); err != nil || got.Pull != p {
			t.Errorf("set %q: %+v err=%v", p, got, err)
		}
	}

	for name, call := range map[string]func() error{
		"an unknown policy": func() error { _, err := s.UpdateSecretPull(ctx, "tnt_x", nil, "DB_DSN", "all"); return err },
		"an empty policy":   func() error { _, err := s.UpdateSecretPull(ctx, "tnt_x", nil, "DB_DSN", ""); return err },
	} {
		if err := call(); !errors.Is(err, ErrInvalidPull) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got, _ := s.LookupSecretMetadata(ctx, "tnt_x", nil, "DB_DSN"); got.Pull != PullNone {
		t.Errorf("a refused update changed the policy to %q", got.Pull)
	}
	if _, err := s.UpdateSecretPull(ctx, "tnt_x", nil, "MISSING", PullAny); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("a missing secret: %v", err)
	}
	if _, err := s.UpdateSecretPull(ctx, "tnt_other", nil, "DB_DSN", PullAny); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("another tenant's secret: %v", err)
	}
	if _, err := s.UpdateSecretPull(ctx, "tnt_x", nil, "bad name", PullAny); !errors.Is(err, ErrInvalidName) {
		t.Errorf("a bad name: %v", err)
	}
	// The database refuses what the store would, should a write ever bypass it.
	if _, err := s.DB.ExecContext(ctx, `UPDATE tenant_secrets SET pull = 'all' WHERE name = 'DB_DSN'`); err == nil {
		t.Error("the column accepted a policy that does not exist")
	}
}

// The policy is per row: a stack's own secret and the tenant-wide one of the
// same name each have theirs.
func TestPullPolicyIsPerScope(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	web := "web"
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API_KEY", "", "a", []byte("tenant-wide")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSecret(ctx, "tnt_x", &web, "API_KEY", "", "a", []byte("web's own")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSecretPull(ctx, "tnt_x", nil, "API_KEY", PullAny); err != nil {
		t.Fatal(err)
	}

	// The stack's own secret wins, and it was never opened.
	meta, err := s.ResolveForRelease(ctx, "tnt_x", "web", "API_KEY")
	if err != nil || meta.Stack == nil || *meta.Stack != "web" || meta.Pull != PullNone {
		t.Fatalf("resolve for web: %+v err=%v", meta, err)
	}
	if v, err := s.DecryptResolved(ctx, meta); err != nil || string(v) != "web's own" {
		t.Errorf("decrypt web's: %q err=%v", v, err)
	}
	// Another stack falls back to the tenant-wide one.
	meta, err = s.ResolveForRelease(ctx, "tnt_x", "ingest", "API_KEY")
	if err != nil || meta.Stack != nil || meta.Pull != PullAny {
		t.Fatalf("resolve for ingest: %+v err=%v", meta, err)
	}
	if v, err := s.DecryptResolved(ctx, meta); err != nil || string(v) != "tenant-wide" {
		t.Errorf("decrypt the tenant-wide one: %q err=%v", v, err)
	}
	if meta, err := s.ResolveForRelease(ctx, "tnt_x", "", "API_KEY"); err != nil || meta.Stack != nil {
		t.Errorf("resolve with no stack: %+v err=%v", meta, err)
	}
	for name, call := range map[string]func() error{
		"a missing secret": func() error { _, err := s.ResolveForRelease(ctx, "tnt_x", "web", "MISSING"); return err },
		"another tenant":   func() error { _, err := s.ResolveForRelease(ctx, "tnt_y", "web", "API_KEY"); return err },
	} {
		if err := call(); !errors.Is(err, ErrSecretNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.ResolveForRelease(ctx, "tnt_x", "web", "../etc"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("a bad name: %v", err)
	}
	if _, err := s.DecryptResolved(ctx, nil); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("decrypt nothing: %v", err)
	}
}

// A release reads the policy from the store every time. The op path caches
// for minutes; a policy that closed a secret must not wait that long.
func TestResolveForReleaseIsNeverCached(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API_KEY", "", "a", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSecretPull(ctx, "tnt_x", nil, "API_KEY", PullAny); err != nil {
		t.Fatal(err)
	}
	// Warm every cache the op path has.
	if _, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "web", "API_KEY"); err != nil {
		t.Fatal(err)
	}
	// Another node changes the row: this store's caches are not told.
	if _, err := s.DB.ExecContext(ctx, `UPDATE tenant_secrets SET pull = 'none' WHERE name = 'API_KEY'`); err != nil {
		t.Fatal(err)
	}
	meta, err := s.ResolveForRelease(ctx, "tnt_x", "web", "API_KEY")
	if err != nil || meta.Pull != PullNone {
		t.Errorf("resolve after the policy closed: %+v err=%v", meta, err)
	}
	// …and the same for a revoked secret.
	if err := s.RevokeSecret(ctx, "tnt_x", nil, "API_KEY"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveForRelease(ctx, "tnt_x", "web", "API_KEY"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("resolve a revoked secret: %v", err)
	}
}

// The decision is made about one version, and that version is what is handed
// over — or nothing is.
func TestDecryptResolvedIsTheVersionThatWasDecidedOn(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API_KEY", "", "a", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	meta, err := s.ResolveForRelease(ctx, "tnt_x", "", "API_KEY")
	if err != nil || meta.VersionNo != 1 {
		t.Fatalf("resolve: %+v err=%v", meta, err)
	}
	if _, err := s.RotateSecret(ctx, "tnt_x", nil, "API_KEY", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if v, err := s.DecryptResolved(ctx, meta); err != nil || string(v) != "v1" {
		t.Errorf("the version decided on: %q err=%v", v, err)
	}
	if fresh, _ := s.ResolveForRelease(ctx, "tnt_x", "", "API_KEY"); fresh.VersionNo != 2 {
		t.Errorf("the next resolve: %+v", fresh)
	}
}

func TestPullTravelsToTheFleetOnlyWhenItSaysSomething(t *testing.T) {
	ctx := context.Background()
	mk := newMockMK(t, 1)
	cap := &captureSyncer{}
	producer := NewStore(newTestDB(t), mk)
	producer.SetSyncer(cap)

	const tenant = "tnt_fleet"
	if _, err := producer.CreateSecret(ctx, tenant, nil, "API_KEY", "", "a", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	// A default policy is left out: a node one release behind has no such
	// column, and a row naming it would fail to apply there.
	last := func() map[string]any { return cap.upserts[len(cap.upserts)-1].parentRow }
	if v, present := last()["pull"]; present {
		t.Errorf("a new secret's row carries pull = %v", v)
	}

	if _, err := producer.UpdateSecretPull(ctx, tenant, nil, "API_KEY", PullReviewed); err != nil {
		t.Fatal(err)
	}
	if len(cap.upserts) != 2 || last()["pull"] != "reviewed" || cap.upserts[1].versionRow != nil {
		t.Fatalf("publish on a policy change: %+v", cap.upserts)
	}
	// Every later write carries it, or the consumer's REPLACE would reset it.
	if _, err := producer.RotateSecret(ctx, tenant, nil, "API_KEY", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if last()["pull"] != "reviewed" {
		t.Errorf("the rotation's row lost the policy: %+v", last())
	}
	if _, err := producer.UpdateSecretDescription(ctx, tenant, nil, "API_KEY", "described"); err != nil {
		t.Fatal(err)
	}
	if last()["pull"] != "reviewed" {
		t.Errorf("the description's row lost the policy: %+v", last())
	}

	// Applied on another node, in order, the row ends where the producer's did.
	consumer := NewStore(newTestDB(t), mk)
	for _, up := range cap.upserts {
		if up.versionRow != nil {
			applyRowLikeApplier(t, consumer.DB, "tenant_secret_versions", jsonRoundTrip(t, up.versionRow))
		}
		applyRowLikeApplier(t, consumer.DB, "tenant_secrets", jsonRoundTrip(t, up.parentRow))
	}
	meta, err := consumer.ResolveForRelease(ctx, tenant, "", "API_KEY")
	if err != nil || meta.Pull != PullReviewed || meta.VersionNo != 2 || meta.Description != "described" {
		t.Fatalf("on the consumer: %+v err=%v", meta, err)
	}
	if v, err := consumer.DecryptResolved(ctx, meta); err != nil || string(v) != "v2" {
		t.Errorf("decrypt on the consumer: %q err=%v", v, err)
	}

	// Closing it again sends a row with no pull, and the REPLACE closes it.
	if _, err := producer.UpdateSecretPull(ctx, tenant, nil, "API_KEY", PullNone); err != nil {
		t.Fatal(err)
	}
	if v, present := last()["pull"]; present {
		t.Errorf("a closed secret's row carries pull = %v", v)
	}
	applyRowLikeApplier(t, consumer.DB, "tenant_secrets", jsonRoundTrip(t, last()))
	if meta, _ := consumer.ResolveForRelease(ctx, tenant, "", "API_KEY"); meta.Pull != PullNone {
		t.Errorf("on the consumer after closing: %+v", meta)
	}

	// The revoke's row carries the policy too; it is the same row map.
	if _, err := producer.UpdateSecretPull(ctx, tenant, nil, "API_KEY", PullAny); err != nil {
		t.Fatal(err)
	}
	if err := producer.RevokeSecret(ctx, tenant, nil, "API_KEY"); err != nil {
		t.Fatal(err)
	}
	if len(cap.revokes) != 1 || cap.revokes[0].parentRow["pull"] != "any" || cap.revokes[0].parentRow["revoked_at"] == nil {
		t.Errorf("the revoke's row: %+v", cap.revokes)
	}
}
