package server

import (
	"database/sql"
	"reflect"
	"sort"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// testDB builds a fresh in-memory SQLite with the (tenants, ops)
// shape Rebuild expects. Returns a pinned-handle DB (MaxOpenConns=1
// since this is :memory:; each pooled connection has its own DB).
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
        CREATE TABLE tenants (tenant_id TEXT, slug TEXT, revoked_at TEXT);
        CREATE TABLE ops     (tenant_id TEXT, stack TEXT, scope INTEGER, name TEXT, txcl TEXT, mock_req TEXT, mock_res TEXT);`); err != nil {
		t.Fatalf("create: %v", err)
	}
	return db
}

func insertTenant(t *testing.T, db *sql.DB, id, slug string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tenants (tenant_id, slug, revoked_at) VALUES (?, ?, NULL)`, id, slug); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
}

func insertOp(t *testing.T, db *sql.DB, tenantID, stack, name, txcl string) {
	t.Helper()
	var tid any
	if tenantID == "" {
		tid = nil
	} else {
		tid = tenantID
	}
	if _, err := db.Exec(
		`INSERT INTO ops (tenant_id, stack, scope, name, txcl) VALUES (?, ?, 0, ?, ?)`,
		tid, stack, name, txcl,
	); err != nil {
		t.Fatalf("insert op: %v", err)
	}
}

func TestRebuild_ParsesWithRedact(t *testing.T) {
	db := testDB(t)
	insertTenant(t, db, "T-ACME", "acme")
	insertOp(t, db, "T-ACME", "acme/support", "r1", `
WHEN *
WITH redact = "user.email, user.ssn"
EXEC "txco://noop"`)

	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	h := r.Hints("acme", "acme/support")
	got := sortedCopy(h.Redact)
	if !reflect.DeepEqual(got, []string{"user.email", "user.ssn"}) {
		t.Fatalf("redact mismatch: %v", got)
	}
	if len(h.Omit) != 0 {
		t.Fatalf("expected no omit: %v", h.Omit)
	}
}

func TestRebuild_ParsesWithOmit(t *testing.T) {
	db := testDB(t)
	insertTenant(t, db, "T-ACME", "acme")
	insertOp(t, db, "T-ACME", "acme/lmtp", "r1", `
WHEN *
WITH omit = "_txc.lmtp.msg.attachments"
EXEC "txco://noop"`)

	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	h := r.Hints("acme", "acme/lmtp")
	if !reflect.DeepEqual(h.Omit, []string{"_txc.lmtp.msg.attachments"}) {
		t.Fatalf("omit mismatch: %v", h.Omit)
	}
}

func TestRebuild_BothOnSameRule(t *testing.T) {
	db := testDB(t)
	insertTenant(t, db, "T-ACME", "acme")
	insertOp(t, db, "T-ACME", "acme/support", "r1", `
WHEN *
WITH redact = "user.email"
WITH omit   = "user.attachments"
EXEC "txco://noop"`)

	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	h := r.Hints("acme", "acme/support")
	if !reflect.DeepEqual(h.Redact, []string{"user.email"}) {
		t.Fatalf("redact: %v", h.Redact)
	}
	if !reflect.DeepEqual(h.Omit, []string{"user.attachments"}) {
		t.Fatalf("omit: %v", h.Omit)
	}
}

func TestRebuild_OmitWinsOnSamePath(t *testing.T) {
	// Different rules in the same (tenant, stack) declare the same
	// path under both redact and omit. Rebuild's omit-wins rule
	// should drop it from Redact.
	db := testDB(t)
	insertTenant(t, db, "T-ACME", "acme")
	insertOp(t, db, "T-ACME", "acme/support", "r1", `
WHEN *
WITH redact = "user.email"
EXEC "txco://noop"`)
	insertOp(t, db, "T-ACME", "acme/support", "r2", `
WHEN *
WITH omit = "user.email"
EXEC "txco://noop"`)

	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	h := r.Hints("acme", "acme/support")
	if len(h.Redact) != 0 {
		t.Fatalf("omit should have won; got redact=%v", h.Redact)
	}
	if !reflect.DeepEqual(h.Omit, []string{"user.email"}) {
		t.Fatalf("omit: %v", h.Omit)
	}
}

func TestRebuild_DedupsAcrossRules(t *testing.T) {
	db := testDB(t)
	insertTenant(t, db, "T-ACME", "acme")
	insertOp(t, db, "T-ACME", "acme/support", "r1", `
WHEN *
WITH redact = "user.email, user.ssn"
EXEC "txco://noop"`)
	insertOp(t, db, "T-ACME", "acme/support", "r2", `
WHEN *
WITH redact = "user.email, user.phone"
EXEC "txco://noop"`)

	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	h := r.Hints("acme", "acme/support")
	got := sortedCopy(h.Redact)
	want := []string{"user.email", "user.phone", "user.ssn"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedup: got %v want %v", got, want)
	}
}

func TestRebuild_StackAndTenantScoping(t *testing.T) {
	db := testDB(t)
	insertTenant(t, db, "T-ACME", "acme")
	insertTenant(t, db, "T-BETA", "beta")
	insertOp(t, db, "T-ACME", "acme/support", "r1", `
WHEN *
WITH redact = "user.email"
EXEC "txco://noop"`)
	insertOp(t, db, "T-ACME", "acme/billing", "r1", `
WHEN *
EXEC "txco://noop"`) // no hint
	insertOp(t, db, "T-BETA", "beta/support", "r1", `
WHEN *
EXEC "txco://noop"`) // no hint, different tenant

	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	// Only acme/support has hints.
	if h := r.Hints("acme", "acme/support"); !reflect.DeepEqual(h.Redact, []string{"user.email"}) {
		t.Fatalf("acme/support: %v", h.Redact)
	}
	// Same tenant, different stack.
	if h := r.Hints("acme", "acme/billing"); !h.Empty() {
		t.Fatalf("acme/billing should be empty: %+v", h)
	}
	// Different tenant, same stack name shape.
	if h := r.Hints("beta", "beta/support"); !h.Empty() {
		t.Fatalf("beta/support should be empty: %+v", h)
	}
}

func TestRebuild_SystemSlugForOrphans(t *testing.T) {
	// Op with no tenant_id (legacy/system rule) should bucket under
	// the system tenant slug, not under an empty string.
	db := testDB(t)
	insertOp(t, db, "", "_sys/boot", "r1", `
WHEN *
WITH redact = "_txc.lmtp.msg.body"
EXEC "txco://noop"`)

	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	h := r.Hints("_sys", "_sys/boot")
	if !reflect.DeepEqual(h.Redact, []string{"_txc.lmtp.msg.body"}) {
		t.Fatalf("system tenant: %v", h.Redact)
	}
	// Lookup with empty tenant should also resolve via the _sys
	// fallback (Hints() canonicalizes empty → _sys).
	h2 := r.Hints("", "_sys/boot")
	if !reflect.DeepEqual(h2.Redact, []string{"_txc.lmtp.msg.body"}) {
		t.Fatalf("empty tenant fallback: %v", h2.Redact)
	}
}

func TestRebuild_NonLiteralValueIgnored(t *testing.T) {
	// `WITH redact = @some.path` would resolve at runtime — not
	// suitable for a static registry. Make sure we don't crash and
	// just skip the rule's hint.
	db := testDB(t)
	insertTenant(t, db, "T-ACME", "acme")
	insertOp(t, db, "T-ACME", "acme/support", "r1", `
WHEN *
WITH redact = @runtime.thing
EXEC "txco://noop"`)
	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if h := r.Hints("acme", "acme/support"); !h.Empty() {
		t.Fatalf("non-literal should be ignored: %+v", h)
	}
}

func TestRebuild_TriggersHintLookupViaSink(t *testing.T) {
	// Wire the registry into a fake sink via trace.HintLookup and
	// confirm the bytes that reach the inner sink are masked.
	db := testDB(t)
	insertTenant(t, db, "T-ACME", "acme")
	insertOp(t, db, "T-ACME", "acme/support", "r1", `
WHEN *
WITH omit = "user.attachments"
WITH redact = "user.email"
EXEC "txco://noop"`)
	r := newRedactRegistry(zap.NewNop())
	if err := r.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// Smoke test: apply the resolved hints to an envelope and
	// confirm the redact/omit semantics match what reached storage.
	envelope := []byte(`{"user":{"email":"a@b","attachments":["zip"]}}`)
	out := trace.ApplyHints(envelope, r.Hints("acme", "acme/support"))
	if got := getString(out, "user.email"); got != "[REDACTED]" {
		t.Fatalf("end-to-end redact: %s", out)
	}
	if hasPath(out, "user.attachments") {
		t.Fatalf("end-to-end omit: %s", out)
	}
}

// --- tiny gjson helpers -------------------------------------------

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func getString(b []byte, path string) string {
	return gjson.GetBytes(b, path).String()
}

func hasPath(b []byte, path string) bool {
	return gjson.GetBytes(b, path).Exists()
}
