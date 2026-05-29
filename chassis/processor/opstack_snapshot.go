package processor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"

	_ "github.com/mattn/go-sqlite3" // sqlite driver for the in-memory snapshot DB
)

// opstackSnapshot is the frozen, tenant-scoped opstack a suspended run
// resumes against. It is serialized into an immutable continuation doc at
// first suspend; resume rebuilds a throwaway in-memory SQLite from it and
// runs the *unchanged* OpsForStage/lookupOpsExact query against it, so a
// later `txco apply` cannot change what an in-flight run executes.
//
// Only the run's own tenant is captured: `ops` rows for that tenant plus
// the single `tenants` row, because lookupOpsExact resolves the tenant via
// `(SELECT tenant_id FROM tenants WHERE slug=?)` against the same DB
// (tenantPredicate). No other tenant's rules ever enter the doc.
type opstackSnapshot struct {
	Ops     []opSnapRow     `json:"ops"`
	Tenants []tenantSnapRow `json:"tenants"`
}

type opSnapRow struct {
	TenantID *string `json:"tenant_id,omitempty"`
	Stack    string  `json:"stack"`
	Scope    int     `json:"scope"`
	Name     string  `json:"name"`
	Txcl     string  `json:"txcl"`
	MockReq  *string `json:"mock_req,omitempty"`
	MockRes  *string `json:"mock_res,omitempty"`
}

type tenantSnapRow struct {
	TenantID  *string `json:"tenant_id,omitempty"`
	Slug      string  `json:"slug"`
	RevokedAt *string `json:"revoked_at,omitempty"`
}

func nullStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}

// snapshotOpstack captures the ops the given tenant can resolve, plus that
// tenant's tenants row, from the request's current opstack DB. Returns the
// canonical JSON, its content hash, and the ops row count (0 ⇒ caller
// skips the snapshot and the run resumes against the live opstack —
// back-compat for untenanted/_sys and empty stacks).
func (pu *Unit) snapshotOpstack(ctx context.Context, tenant string) ([]byte, string, int, error) {
	db := pu.opstackDB(ctx)
	tenantPred, tenantArgs := tenantPredicate(tenant)

	var snap opstackSnapshot

	oRows, err := db.QueryContext(ctx,
		"SELECT tenant_id, stack, scope, name, txcl, mock_req, mock_res FROM ops WHERE 1=1"+tenantPred,
		tenantArgs...)
	if err != nil {
		return nil, "", 0, err
	}
	defer oRows.Close()
	for oRows.Next() {
		var (
			tid, mreq, mres sql.NullString
			r               opSnapRow
		)
		if err := oRows.Scan(&tid, &r.Stack, &r.Scope, &r.Name, &r.Txcl, &mreq, &mres); err != nil {
			return nil, "", 0, err
		}
		r.TenantID, r.MockReq, r.MockRes = nullStr(tid), nullStr(mreq), nullStr(mres)
		snap.Ops = append(snap.Ops, r)
	}
	if err := oRows.Err(); err != nil {
		return nil, "", 0, err
	}

	if tenant != "" {
		tRows, err := db.QueryContext(ctx,
			"SELECT tenant_id, slug, revoked_at FROM tenants WHERE slug = ?", tenant)
		if err != nil {
			return nil, "", 0, err
		}
		defer tRows.Close()
		for tRows.Next() {
			var tid, rev sql.NullString
			var r tenantSnapRow
			if err := tRows.Scan(&tid, &r.Slug, &rev); err != nil {
				return nil, "", 0, err
			}
			r.TenantID, r.RevokedAt = nullStr(tid), nullStr(rev)
			snap.Tenants = append(snap.Tenants, r)
		}
		if err := tRows.Err(); err != nil {
			return nil, "", 0, err
		}
	}

	data, err := json.Marshal(&snap)
	if err != nil {
		return nil, "", 0, err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), len(snap.Ops), nil
}

// buildSnapshotDB rebuilds an in-memory SQLite holding exactly the frozen
// opstack. The schema mirrors only the columns the resume-path queries
// touch (lookupOpsExact, buildOpstack, tenantExists). MaxOpenConns(1) is
// mandatory: go-sqlite3 gives each pooled connection its own private
// `:memory:` DB, so without pinning a second connection would see an
// empty schema (same rationale as dbcache.New).
func buildSnapshotDB(data []byte) (*sql.DB, error) {
	var snap opstackSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(
		`CREATE TABLE ops (tenant_id TEXT, stack TEXT, scope INTEGER, name TEXT, txcl TEXT, mock_req TEXT, mock_res TEXT);
		 CREATE TABLE tenants (tenant_id TEXT, slug TEXT, revoked_at TEXT);`); err != nil {
		db.Close()
		return nil, err
	}
	for _, r := range snap.Ops {
		if _, err := db.Exec(
			`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (?,?,?,?,?,?,?)`,
			r.TenantID, r.Stack, r.Scope, r.Name, r.Txcl, r.MockReq, r.MockRes); err != nil {
			db.Close()
			return nil, err
		}
	}
	for _, r := range snap.Tenants {
		if _, err := db.Exec(
			`INSERT INTO tenants (tenant_id, slug, revoked_at) VALUES (?,?,?)`,
			r.TenantID, r.Slug, r.RevokedAt); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}
