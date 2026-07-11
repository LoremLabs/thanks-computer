package secrets

import (
	"context"
	"testing"
)

// TestMaterializeCacheHitSkipsDB proves a repeat materialize serves from the
// encrypted cache with zero DB reads: the version row is corrupted behind the
// cache's back, and the hit still decrypts. Flushing (what the dbcache reload
// hook does) restores authoritative reads, which then see the corruption.
func TestMaterializeCacheHitSkipsDB(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API_KEY", "", "a", []byte("v1")); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	pt, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "API_KEY")
	if err != nil || string(pt) != "v1" {
		t.Fatalf("first materialize = %q, %v; want v1", pt, err)
	}

	if _, err := s.DB.Exec(`UPDATE tenant_secret_versions SET ciphertext = X'00'`); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	pt, _, err = s.MaterializeSecretForOp(ctx, "tnt_x", "", "API_KEY")
	if err != nil || string(pt) != "v1" {
		t.Fatalf("cached materialize = %q, %v; want v1 (hit must not read the DB)", pt, err)
	}

	s.FlushMaterializeCache()
	if _, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "API_KEY"); err == nil {
		t.Fatal("materialize after flush succeeded — expected the authoritative read to see the corrupted row")
	}
}

// TestMaterializeCacheFlushedByRotate proves a local rotation invalidates
// synchronously — the very next materialize returns the NEW value, matching
// pre-cache open-core semantics.
func TestMaterializeCacheFlushedByRotate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API_KEY", "", "a", []byte("v1")); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
	if pt, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "API_KEY"); err != nil || string(pt) != "v1" {
		t.Fatalf("materialize = %q, %v; want v1", pt, err)
	}
	if _, err := s.RotateSecret(ctx, "tnt_x", nil, "API_KEY", []byte("v2")); err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	pt, meta, err := s.MaterializeSecretForOp(ctx, "tnt_x", "", "API_KEY")
	if err != nil || string(pt) != "v2" {
		t.Fatalf("materialize after rotate = %q, %v; want v2 (stale cache?)", pt, err)
	}
	if meta.VersionNo != 2 {
		t.Errorf("VersionNo = %d, want 2", meta.VersionNo)
	}
}

// TestMaterializeCacheScopeFallbackInvalidatedByCreate proves the fallback
// memoization is safe: a tenant-wide value cached under a stack-scoped
// request key is dropped when a stack-scoped secret is created over it.
func TestMaterializeCacheScopeFallbackInvalidatedByCreate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	stack := "www"

	if _, err := s.CreateSecret(ctx, "tnt_x", nil, "API_KEY", "", "a", []byte("wide")); err != nil {
		t.Fatalf("CreateSecret wide: %v", err)
	}
	if pt, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", stack, "API_KEY"); err != nil || string(pt) != "wide" {
		t.Fatalf("fallback materialize = %q, %v; want wide", pt, err)
	}
	if _, err := s.CreateSecret(ctx, "tnt_x", &stack, "API_KEY", "", "a", []byte("scoped")); err != nil {
		t.Fatalf("CreateSecret scoped: %v", err)
	}
	if pt, _, err := s.MaterializeSecretForOp(ctx, "tnt_x", stack, "API_KEY"); err != nil || string(pt) != "scoped" {
		t.Fatalf("materialize after scoped create = %q, %v; want scoped (stale fallback cached?)", pt, err)
	}
}
