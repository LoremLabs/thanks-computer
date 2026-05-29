package tenants

import (
	"context"
	"strings"
	"testing"
)

const tNow = "2026-05-19T00:00:00Z"

func countHostnames(t *testing.T, s *Store, tenantID, stack string) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(
		`SELECT count(*) FROM tenant_hostnames
		  WHERE tenant_id=? AND stack=? AND created_by=? AND revoked_at IS NULL`,
		tenantID, stack, SystemStructuredHostCreatedBy).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestEnsureSystemHostnameTx_MintAndReuse(t *testing.T) {
	s, db := newTestStore(t)
	ctx := context.Background()

	mint := func(tenant, stack, suffix string) string {
		t.Helper()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		h, err := EnsureSystemHostnameTx(ctx, tx, tenant, stack, suffix, tNow)
		if err != nil {
			_ = tx.Rollback()
			t.Fatalf("EnsureSystemHostnameTx: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return h
	}

	// First activation mints.
	h1 := mint("tnt_x", "test-stack", ".localhost")
	if !strings.HasPrefix(h1, "test-stack-") || !strings.HasSuffix(h1, ".localhost") {
		t.Fatalf("host %q: want test-stack-<rand>.localhost", h1)
	}
	if got := countHostnames(t, s, "tnt_x", "test-stack"); got != 1 {
		t.Fatalf("row count after first mint = %d, want 1", got)
	}
	// verified_at + created_by stamped (so it routes under strict mode).
	var createdBy, verifiedAt string
	if err := db.QueryRow(
		`SELECT created_by, COALESCE(verified_at,'') FROM tenant_hostnames WHERE hostname=?`,
		h1).Scan(&createdBy, &verifiedAt); err != nil {
		t.Fatalf("row read: %v", err)
	}
	if createdBy != SystemStructuredHostCreatedBy || verifiedAt != tNow {
		t.Fatalf("created_by=%q verified_at=%q; want sentinel + %q", createdBy, verifiedAt, tNow)
	}

	// Re-activation reuses the SAME hostname, no new row.
	h2 := mint("tnt_x", "test-stack", ".localhost")
	if h2 != h1 {
		t.Fatalf("reuse failed: %q != %q", h2, h1)
	}
	if got := countHostnames(t, s, "tnt_x", "test-stack"); got != 1 {
		t.Fatalf("row count after re-activation = %d, want 1 (idempotent)", got)
	}

	// Different stack → different row/host.
	h3 := mint("tnt_x", "other", ".localhost")
	if h3 == h1 || !strings.HasPrefix(h3, "other-") {
		t.Fatalf("other stack host %q overlaps or mis-hinted", h3)
	}

	// Missing leading dot on the suffix is tolerated.
	h4 := mint("tnt_x", "dotless", "localhost")
	if !strings.HasSuffix(h4, ".localhost") {
		t.Fatalf("dotless suffix not normalized: %q", h4)
	}
}

func TestEnsureSystemHostnameTx_EmptySuffixIsNoop(t *testing.T) {
	_, db := newTestStore(t)
	ctx := context.Background()
	tx, _ := db.BeginTx(ctx, nil)
	defer func() { _ = tx.Rollback() }()
	h, err := EnsureSystemHostnameTx(ctx, tx, "tnt_x", "web", "", tNow)
	if err != nil || h != "" {
		t.Fatalf("empty suffix: got (%q,%v), want (\"\",nil)", h, err)
	}
}
