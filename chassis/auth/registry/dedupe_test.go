package registry

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"
)

// TestCreateKeyDuplicateRejected — once the UNIQUE index exists,
// CreateKey returns ErrKeyAlreadyEnrolled on a second insert of the
// same public_key. The test schema doesn't ship the index by default
// (so older tests don't have to know about phase 4); we add it here.
func TestCreateKeyDuplicateRejected(t *testing.T) {
	db := newRegistryDB(t)
	if _, err := db.Exec(
		`CREATE UNIQUE INDEX actor_keys_public_key_idx
		   ON actor_keys(public_key) WHERE revoked_at IS NULL`); err != nil {
		t.Fatalf("create unique index: %v", err)
	}
	r := New(db, nil)
	ctx := context.Background()

	pub, _, _ := ed25519.GenerateKey(nil)
	if err := r.CreateActor(ctx, Actor{ActorID: "actor_a"}); err != nil {
		t.Fatalf("CreateActor a: %v", err)
	}
	if err := r.CreateActor(ctx, Actor{ActorID: "actor_b"}); err != nil {
		t.Fatalf("CreateActor b: %v", err)
	}
	if err := r.CreateKey(ctx, Key{KeyID: "key_1", ActorID: "actor_a", PublicKey: pub}); err != nil {
		t.Fatalf("first CreateKey: %v", err)
	}
	err := r.CreateKey(ctx, Key{KeyID: "key_2", ActorID: "actor_b", PublicKey: pub})
	if !errors.Is(err, ErrKeyAlreadyEnrolled) {
		t.Fatalf("second CreateKey: got %v, want ErrKeyAlreadyEnrolled", err)
	}
}

// TestLookupKeyByPublicKeyRoundtrip — Insert + Lookup returns the row.
// Revoked rows are invisible to the lookup so phase-4 dedupe doesn't
// trip on stale data.
func TestLookupKeyByPublicKeyRoundtrip(t *testing.T) {
	r := New(newRegistryDB(t), nil)
	ctx := context.Background()

	pub, _, _ := ed25519.GenerateKey(nil)
	if err := r.CreateActor(ctx, Actor{ActorID: "actor_a"}); err != nil {
		t.Fatalf("CreateActor: %v", err)
	}
	if err := r.CreateKey(ctx, Key{KeyID: "key_1", ActorID: "actor_a", PublicKey: pub}); err != nil {
		t.Fatalf("CreateKey: %v", err)
	}

	got, err := r.LookupKeyByPublicKey(ctx, pub)
	if err != nil {
		t.Fatalf("LookupKeyByPublicKey: %v", err)
	}
	if got.KeyID != "key_1" || got.ActorID != "actor_a" {
		t.Errorf("unexpected row: %+v", got)
	}

	// Revoke; lookup should now miss.
	if err := r.RevokeKey(ctx, "key_1"); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}
	if _, err := r.LookupKeyByPublicKey(ctx, pub); !errors.Is(err, ErrNotFound) {
		t.Errorf("after revoke: got %v, want ErrNotFound", err)
	}
}

// TestConsumeInvitationReusesExistingPrincipal — two invitations into
// DIFFERENT tenants, redeemed with the same pubkey, should yield ONE
// actor row with TWO memberships. This is the canonical "alice joins
// a second tenant" workflow.
func TestConsumeInvitationReusesExistingPrincipal(t *testing.T) {
	db := newRegistryDB(t)
	r := New(db, dbSlugLookup(db))
	ctx := context.Background()

	// Seed two tenants beyond default.
	for _, tnt := range []struct{ id, slug string }{{"tnt_a", "alpha"}, {"tnt_b", "bravo"}} {
		seedTenant(t, db, tnt.id, tnt.slug)
	}
	// Inviting actor must exist (CreateInvitation requires created_by).
	if err := r.CreateActor(ctx, Actor{ActorID: "actor_admin"}); err != nil {
		t.Fatalf("CreateActor admin: %v", err)
	}

	mkInv := func(token, invID, tenantID string) {
		if err := r.CreateInvitation(ctx, Invitation{
			InvitationID: invID,
			TokenHash:    HashToken(token),
			Label:        "alice",
			TenantID:     tenantID,
			Capabilities: []string{"admin:all"},
			CreatedBy:    "actor_admin",
			ExpiresAt:    time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateInvitation %s: %v", invID, err)
		}
	}
	mkInv("tok_a", "inv_a", "tnt_a")
	mkInv("tok_b", "inv_b", "tnt_b")

	pub, _, _ := ed25519.GenerateKey(nil)

	// First consume: fresh principal in alpha.
	first, err := r.ConsumeInvitation(ctx, HashToken("tok_a"), "actor_alice", "key_alice", pub, "alice", "human")
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if first.Reused {
		t.Errorf("first consume should not be Reused")
	}
	if first.ActorID != "actor_alice" {
		t.Errorf("first consume ActorID = %q, want actor_alice", first.ActorID)
	}

	// Second consume: same pubkey, different tenant. Reuse alice's actor.
	second, err := r.ConsumeInvitation(ctx, HashToken("tok_b"), "actor_should_not_use", "key_should_not_use", pub, "alice", "human")
	if err != nil {
		t.Fatalf("second consume: %v", err)
	}
	if !second.Reused {
		t.Errorf("second consume should be Reused")
	}
	if second.ActorID != "actor_alice" {
		t.Errorf("second consume ActorID = %q, want actor_alice (reuse)", second.ActorID)
	}
	if second.KeyID != "key_alice" {
		t.Errorf("second consume KeyID = %q, want key_alice (reuse)", second.KeyID)
	}

	// One actor, one key, two memberships.
	actors, err := r.ListActors(ctx)
	if err != nil {
		t.Fatalf("ListActors: %v", err)
	}
	aliceCount := 0
	for _, a := range actors {
		if a.ActorID == "actor_alice" {
			aliceCount++
		}
		if a.ActorID == "actor_should_not_use" {
			t.Errorf("dup actor was inserted!")
		}
	}
	if aliceCount != 1 {
		t.Errorf("expected exactly one alice actor row, got %d", aliceCount)
	}

	ms, err := r.ListMembershipsForActor(ctx, "actor_alice")
	if err != nil {
		t.Fatalf("ListMembershipsForActor: %v", err)
	}
	if len(ms) != 2 {
		t.Fatalf("expected 2 memberships, got %d: %+v", len(ms), ms)
	}
	slugs := map[string]bool{}
	for _, m := range ms {
		slugs[m.TenantSlug] = true
	}
	if !slugs["alpha"] || !slugs["bravo"] {
		t.Errorf("expected memberships in both alpha and bravo; got slugs %v", slugs)
	}
}

// TestConsumeInvitationFreshGrantsMembership — even when no
// deduplication happens (fresh pubkey), phase 4 writes a membership
// row in the invitation's tenant. The tenant middleware reads it on
// the next signed request to scope capabilities.
func TestConsumeInvitationFreshGrantsMembership(t *testing.T) {
	db := newRegistryDB(t)
	r := New(db, dbSlugLookup(db))
	ctx := context.Background()
	seedTenant(t, db, "tnt_lorem", "loremlabs")
	if err := r.CreateActor(ctx, Actor{ActorID: "actor_admin"}); err != nil {
		t.Fatalf("CreateActor admin: %v", err)
	}
	if err := r.CreateInvitation(ctx, Invitation{
		InvitationID: "inv_x", TokenHash: HashToken("tok"),
		TenantID: "tnt_lorem", Capabilities: []string{"admin:all"},
		CreatedBy: "actor_admin", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	got, err := r.ConsumeInvitation(ctx, HashToken("tok"), "actor_new", "key_new", pub, "n", "human")
	if err != nil {
		t.Fatalf("ConsumeInvitation: %v", err)
	}
	if got.TenantID != "tnt_lorem" {
		t.Errorf("TenantID = %q, want tnt_lorem", got.TenantID)
	}
	m, err := r.LoadMembership(ctx, "actor_new", "tnt_lorem")
	if err != nil {
		t.Fatalf("LoadMembership: %v", err)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != "admin:all" {
		t.Errorf("membership caps = %v, want [admin:all]", m.Capabilities)
	}
}
