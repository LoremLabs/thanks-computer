package connection

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// Store reads and writes the two connection tables in the auth database
// (db/schema/*/auth/0009_connections.sql): `connections`, one row per
// identity, holding only the current material; and `connection_flows`, a
// sign-in in progress, consumed once.
//
// Not the tenant secret store: a secret is the operator's, versioned
// forever and fed to every node; a connection is a principal's, renewed
// every few hours on whichever node uses it, in the shared auth database.
type Store struct {
	DB      *sql.DB
	Dialect registry.Dialect
	MK      secrets.MasterKeyProvider

	now func() time.Time
}

// Row statuses.
const (
	StatusActive    = "active"
	StatusUncertain = "uncertain" // a renewal's outcome is unknown
	StatusReconnect = "reconnect" // the provider refused the refresh token
)

var (
	// ErrNotFound: the principal has no such connection.
	ErrNotFound = errors.New("connection: not found")
	// ErrFlowGone: no such flow, or it was consumed or has expired.
	ErrFlowGone = errors.New("connection: no such sign-in, or it was already used")
	// ErrFenced: the row moved on (another node renewed it, or the claim
	// was not this writer's).
	ErrFenced = errors.New("connection: write fenced")
)

// NewStore builds a Store over an open, migrated auth DB. A nil dialect is
// SQLite. mk seals and opens material; nil means every open fails.
func NewStore(db *sql.DB, d registry.Dialect, mk secrets.MasterKeyProvider) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{DB: db, Dialect: d, MK: mk, now: time.Now}
}

// SetClock replaces the store's clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Row mirrors a connections row. The material is not in it: Open returns
// it, to the one caller that sends it.
type Row struct {
	ID               string
	TenantID         string
	PrincipalID      string
	Provider         string
	Label            string
	Kind             string
	Subject          string
	Account          string
	Status           string
	Scopes           []string
	Generation       int64
	ExpiresAt        *time.Time
	LifetimeS        int64
	RefreshExpiresAt *time.Time
	ClaimID          string
	ClaimUntil       *time.Time
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time

	material string
}

// Flow mirrors a connection_flows row.
type Flow struct {
	NonceHash   string
	TenantID    string
	PrincipalID string
	Provider    string
	Label       string
	Kind        string
	Stack       string
	RedirectURI string
	BrowserHash string
	Initiator   string
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
}

func (s *Store) stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func stampPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func parseStamp(v string) time.Time {
	t, _ := time.Parse(time.RFC3339, v)
	return t
}

func parseStampPtr(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseStamp(v.String)
	return &t
}

func (s *Store) ex(ctx context.Context, query string, a ...any) (sql.Result, error) {
	return s.DB.ExecContext(ctx, s.Dialect.Rebind(query), a...)
}
func (s *Store) qy(ctx context.Context, query string, a ...any) (*sql.Rows, error) {
	return s.DB.QueryContext(ctx, s.Dialect.Rebind(query), a...)
}
func (s *Store) qr(ctx context.Context, query string, a ...any) *sql.Row {
	return s.DB.QueryRowContext(ctx, s.Dialect.Rebind(query), a...)
}

// ── flows ────────────────────────────────────────────────────────────────

// PutFlow records a sign-in that has started.
func (s *Store) PutFlow(ctx context.Context, f Flow) error {
	_, err := s.ex(ctx, `INSERT INTO connection_flows
		 (nonce_hash, tenant_id, principal_id, provider, label, kind, stack, redirect_uri, browser_hash, initiator, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.NonceHash, f.TenantID, f.PrincipalID, f.Provider, f.Label, f.Kind, f.Stack, f.RedirectURI, f.BrowserHash, f.Initiator, s.stamp(f.ExpiresAt))
	return err
}

// ConsumeFlow marks the flow used and returns it. Exactly one caller gets
// it: a replayed state, a racing second callback, or an expired flow is
// ErrFlowGone. The code is exchanged only after this.
func (s *Store) ConsumeFlow(ctx context.Context, nonceHash string) (Flow, error) {
	now := s.stamp(s.now())
	res, err := s.ex(ctx, `UPDATE connection_flows SET consumed_at = ?
		 WHERE nonce_hash = ? AND consumed_at IS NULL AND expires_at > ?`, now, nonceHash, now)
	if err != nil {
		return Flow{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Flow{}, ErrFlowGone
	}
	var (
		f        Flow
		expires  string
		consumed sql.NullString
	)
	err = s.qr(ctx, `SELECT nonce_hash, tenant_id, principal_id, provider, label, kind, stack, redirect_uri, browser_hash, initiator, expires_at, consumed_at
		 FROM connection_flows WHERE nonce_hash = ?`, nonceHash).Scan(
		&f.NonceHash, &f.TenantID, &f.PrincipalID, &f.Provider, &f.Label, &f.Kind, &f.Stack, &f.RedirectURI, &f.BrowserHash, &f.Initiator, &expires, &consumed)
	if err != nil {
		return Flow{}, err
	}
	f.ExpiresAt = parseStamp(expires)
	f.ConsumedAt = parseStampPtr(consumed)
	return f, nil
}

// SweepFlows deletes flows past their expiry (consumed or not). Every node
// may run it; the delete is idempotent.
func (s *Store) SweepFlows(ctx context.Context) (int64, error) {
	res, err := s.ex(ctx, `DELETE FROM connection_flows WHERE expires_at <= ?`, s.stamp(s.now()))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ── rows ─────────────────────────────────────────────────────────────────

const rowCols = `id, tenant_id, principal_id, provider, label, kind, subject, account, status, scopes,
	generation, expires_at, lifetime_s, refresh_expires_at, claim_id, claim_until, material,
	created_by, created_at, updated_at`

func scanRow(sc interface{ Scan(...any) error }) (Row, error) {
	var (
		r                                      Row
		scopes, created, updated               string
		expires, refreshExp, claimID, claimTil sql.NullString
	)
	err := sc.Scan(&r.ID, &r.TenantID, &r.PrincipalID, &r.Provider, &r.Label, &r.Kind, &r.Subject, &r.Account, &r.Status, &scopes,
		&r.Generation, &expires, &r.LifetimeS, &refreshExp, &claimID, &claimTil, &r.material,
		&r.CreatedBy, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	if err != nil {
		return Row{}, err
	}
	_ = json.Unmarshal([]byte(scopes), &r.Scopes)
	r.ExpiresAt = parseStampPtr(expires)
	r.RefreshExpiresAt = parseStampPtr(refreshExp)
	r.ClaimID = claimID.String
	r.ClaimUntil = parseStampPtr(claimTil)
	r.CreatedAt = parseStamp(created)
	r.UpdatedAt = parseStamp(updated)
	return r, nil
}

// Get is one connection by its identity.
func (s *Store) Get(ctx context.Context, tenantID, principalID, provider, label string) (Row, error) {
	return scanRow(s.qr(ctx, `SELECT `+rowCols+` FROM connections
		 WHERE tenant_id = ? AND principal_id = ? AND provider = ? AND label = ?`, tenantID, principalID, provider, label))
}

// Reload re-reads a row by id (after a claim, to see what the claimant
// wrote). ErrNotFound when it was deleted meanwhile.
func (s *Store) Reload(ctx context.Context, id string) (Row, error) {
	return scanRow(s.qr(ctx, `SELECT `+rowCols+` FROM connections WHERE id = ?`, id))
}

// Open decrypts a row's material. The caller zeroes nothing: the strings
// are short-lived values on one request.
func (s *Store) Open(ctx context.Context, r Row) (Material, error) {
	if s.MK == nil {
		return Material{}, errors.New("connection: no master key")
	}
	return openMaterial(s.MK, r.material, r.TenantID, r.PrincipalID, r.Provider, r.Label, r.ID, r.Generation)
}

// NewRow is what a finished sign-in stores.
type NewRow struct {
	TenantID, PrincipalID, Provider, Label, Kind string
	Subject, Account                             string
	Scopes                                       []string
	ExpiresAt, RefreshExpiresAt                  *time.Time
	LifetimeS                                    int64
	CreatedBy                                    string
}

// Put stores a connection: a new row, or the identity's existing row
// re-sealed at the next generation with status active and no claim. A
// reconnect replaces whatever was there.
func (s *Store) Put(ctx context.Context, in NewRow, m Material) (Row, error) {
	if s.MK == nil {
		return Row{}, errors.New("connection: no master key")
	}
	scopes, _ := json.Marshal(append([]string{}, in.Scopes...))
	for attempt := 0; ; attempt++ {
		now := s.now()
		existing, err := s.Get(ctx, in.TenantID, in.PrincipalID, in.Provider, in.Label)
		switch {
		case err == nil:
			gen := existing.Generation + 1
			sealedCol, serr := sealMaterial(s.MK, m, in.TenantID, in.PrincipalID, in.Provider, in.Label, existing.ID, gen)
			if serr != nil {
				return Row{}, serr
			}
			res, uerr := s.ex(ctx, `UPDATE connections SET kind = ?, subject = ?, account = ?, status = ?, scopes = ?,
				 generation = ?, expires_at = ?, lifetime_s = ?, refresh_expires_at = ?, claim_id = NULL, claim_until = NULL,
				 material = ?, updated_at = ?
				 WHERE id = ? AND generation = ?`,
				in.Kind, in.Subject, in.Account, StatusActive, string(scopes),
				gen, stampPtr(in.ExpiresAt), in.LifetimeS, stampPtr(in.RefreshExpiresAt),
				sealedCol, s.stamp(now), existing.ID, existing.Generation)
			if uerr != nil {
				return Row{}, uerr
			}
			if n, _ := res.RowsAffected(); n == 1 {
				return s.Reload(ctx, existing.ID)
			}
		case errors.Is(err, ErrNotFound):
			id := "con_" + hxid.NewTimeSort().String()
			sealedCol, serr := sealMaterial(s.MK, m, in.TenantID, in.PrincipalID, in.Provider, in.Label, id, 1)
			if serr != nil {
				return Row{}, serr
			}
			_, ierr := s.ex(ctx, `INSERT INTO connections
				 (id, tenant_id, principal_id, provider, label, kind, subject, account, status, scopes,
				  generation, expires_at, lifetime_s, refresh_expires_at, material, created_by, created_at, updated_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?)`,
				id, in.TenantID, in.PrincipalID, in.Provider, in.Label, in.Kind, in.Subject, in.Account, StatusActive, string(scopes),
				stampPtr(in.ExpiresAt), in.LifetimeS, stampPtr(in.RefreshExpiresAt), sealedCol, in.CreatedBy, s.stamp(now), s.stamp(now))
			if ierr == nil {
				return s.Reload(ctx, id)
			}
			if !s.Dialect.IsUniqueViolationGeneric(ierr) {
				return Row{}, ierr
			}
		default:
			return Row{}, err
		}
		// A lost race either way: read again, once.
		if attempt > 0 {
			return Row{}, ErrFenced
		}
	}
}

// ListByPrincipal is every connection of a principal, by provider.
func (s *Store) ListByPrincipal(ctx context.Context, tenantID, principalID string) ([]Row, error) {
	return s.rows(ctx, `SELECT `+rowCols+` FROM connections WHERE tenant_id = ? AND principal_id = ? ORDER BY provider, label`, tenantID, principalID)
}

// ListByProvider is every connection at a provider, by principal.
func (s *Store) ListByProvider(ctx context.Context, tenantID, provider string) ([]Row, error) {
	return s.rows(ctx, `SELECT `+rowCols+` FROM connections WHERE tenant_id = ? AND provider = ? ORDER BY principal_id, label`, tenantID, provider)
}

func (s *Store) rows(ctx context.Context, query string, args ...any) ([]Row, error) {
	rows, err := s.qy(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Delete removes a connection. It returns the row it removed, for the
// revocation at the provider, and false when there was none.
func (s *Store) Delete(ctx context.Context, tenantID, principalID, provider, label string) (Row, bool, error) {
	r, err := s.Get(ctx, tenantID, principalID, provider, label)
	if errors.Is(err, ErrNotFound) {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	res, err := s.ex(ctx, `DELETE FROM connections WHERE id = ?`, r.ID)
	if err != nil {
		return Row{}, false, err
	}
	n, _ := res.RowsAffected()
	return r, n > 0, nil
}

// DeletePrincipal removes every connection of a principal (the principal
// is gone).
func (s *Store) DeletePrincipal(ctx context.Context, tenantID, principalID string) (int64, error) {
	res, err := s.ex(ctx, `DELETE FROM connections WHERE tenant_id = ? AND principal_id = ?`, tenantID, principalID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ── renewal ──────────────────────────────────────────────────────────────

// Claim takes the row's renewal for claimID until `until`. It succeeds for
// one caller: the row at this generation, in the status the caller saw
// (active; or uncertain, when the provider's rule allows a retry), with no
// claim at all. An expired claim does not count as none — nobody takes
// over a renewal whose outcome is unknown (ReleaseStale is what clears
// one, as uncertain).
func (s *Store) Claim(ctx context.Context, id string, generation int64, claimID string, until time.Time, status string) (bool, error) {
	res, err := s.ex(ctx, `UPDATE connections SET claim_id = ?, claim_until = ?
		 WHERE id = ? AND generation = ? AND status = ? AND claim_id IS NULL`,
		claimID, s.stamp(until), id, generation, status)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Renew writes a renewal's result, fenced by the claim and the generation:
// a late writer whose claim was cleared, or whose generation moved on, is
// ErrFenced and changes nothing.
func (s *Store) Renew(ctx context.Context, r Row, claimID string, m Material, expiresAt, refreshExpiresAt *time.Time, lifetimeS int64) (Row, error) {
	if s.MK == nil {
		return Row{}, errors.New("connection: no master key")
	}
	gen := r.Generation + 1
	sealedCol, err := sealMaterial(s.MK, m, r.TenantID, r.PrincipalID, r.Provider, r.Label, r.ID, gen)
	if err != nil {
		return Row{}, err
	}
	res, err := s.ex(ctx, `UPDATE connections SET generation = ?, material = ?, expires_at = ?, lifetime_s = ?, refresh_expires_at = ?,
		 status = ?, claim_id = NULL, claim_until = NULL, updated_at = ?
		 WHERE id = ? AND generation = ? AND claim_id = ?`,
		gen, sealedCol, stampPtr(expiresAt), lifetimeS, stampPtr(refreshExpiresAt), StatusActive, s.stamp(s.now()),
		r.ID, r.Generation, claimID)
	if err != nil {
		return Row{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Row{}, ErrFenced
	}
	return s.Reload(ctx, r.ID)
}

// SetStatus writes a renewal's verdict (reconnect, uncertain) under the
// claim, and clears it. Fenced like Renew.
func (s *Store) SetStatus(ctx context.Context, r Row, claimID, status string) error {
	res, err := s.ex(ctx, `UPDATE connections SET status = ?, claim_id = NULL, claim_until = NULL, updated_at = ?
		 WHERE id = ? AND generation = ? AND claim_id = ?`,
		status, s.stamp(s.now()), r.ID, r.Generation, claimID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrFenced
	}
	return nil
}

// Release clears a claim whose request was never sent: nothing changed, the
// next use may claim again.
func (s *Store) Release(ctx context.Context, r Row, claimID string) error {
	_, err := s.ex(ctx, `UPDATE connections SET claim_id = NULL, claim_until = NULL
		 WHERE id = ? AND generation = ? AND claim_id = ?`, r.ID, r.Generation, claimID)
	return err
}

// ReleaseStale clears a claim past its lease and marks the row uncertain:
// the claimant may have died after sending the refresh token. It fences on
// the stale claim's id, so a claimant that finishes late is fenced too.
func (s *Store) ReleaseStale(ctx context.Context, r Row) (bool, error) {
	if r.ClaimID == "" || r.ClaimUntil == nil || !r.ClaimUntil.Before(s.now()) {
		return false, nil
	}
	res, err := s.ex(ctx, `UPDATE connections SET status = ?, claim_id = NULL, claim_until = NULL, updated_at = ?
		 WHERE id = ? AND generation = ? AND claim_id = ?`,
		StatusUncertain, s.stamp(s.now()), r.ID, r.Generation, r.ClaimID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// String is the identity a log line or error names: never material.
func (r Row) String() string {
	id := r.Provider
	if r.Label != "" {
		id += "/" + r.Label
	}
	return fmt.Sprintf("%s %s %s", r.PrincipalID, id, strings.TrimSpace(r.Status))
}
