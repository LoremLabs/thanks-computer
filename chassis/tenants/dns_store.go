package tenants

// DNS zone + record CRUD on the tenants Store. Mirrors the hostname Tx
// pattern in store_tx.go: callers pre-generate IDs (so the fleet-sync
// producer hook can include them in its pre-tx artifact), the methods
// canonicalize/validate, and timestamps are RFC3339 UTC text.
//
// A "delegated zone" is a dns_zones row. mode='pattern' (default) →
// the dns head synthesizes the fixed record shape and lets dns_records
// override it; mode='manual' → materialized-only. See
// internal docs/todo-dns-authority.md.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// ErrZoneExists signals an active dns_zones row already covers the origin.
var ErrZoneExists = errors.New("dns zone already exists")

// SystemZoneHostCreatedBy marks a tenant_hostnames row auto-minted for a
// stack under the tenant's delegated DNS zone (`stack-name.<origin>`).
// Distinct from SystemStructuredHostCreatedBy so the global-suffix and
// delegated-zone mechanisms coexist without colliding.
const SystemZoneHostCreatedBy = "system:dns-zone-host"

// ActivePatternZoneOriginTx returns the tenant's active pattern-mode
// zone origin (byte-order first when several exist), or ok=false. Read
// inside the activation tx so the minted routing host is consistent
// with the same transaction's view of the zone set.
//
// The pick happens in Go, not via ORDER BY … LIMIT 1: a SQL text sort
// follows the database collation on Postgres but byte order on SQLite,
// so with 2+ zones the two engines could mint DIFFERENT hostnames for
// the same stack (and a SQLite→Postgres migration would flip the pick).
// Go string comparison is byte order on both.
func ActivePatternZoneOriginTx(ctx context.Context, tx *sql.Tx, tenantID string, d registry.Dialect) (string, bool, error) {
	rows, err := tx.QueryContext(ctx,
		orSQLite(d).Rebind(`SELECT origin FROM dns_zones
		  WHERE tenant_id = ? AND mode = 'pattern'
		    AND revoked_at IS NULL AND verified_at IS NOT NULL`), tenantID)
	if err != nil {
		return "", false, err
	}
	defer rows.Close()
	best := ""
	for rows.Next() {
		var origin string
		if err := rows.Scan(&origin); err != nil {
			return "", false, err
		}
		if origin != "" && (best == "" || origin < best) {
			best = origin
		}
	}
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	return best, best != "", nil
}

// DomainCoveredByZone reports whether `domain` (apex or any subdomain) falls
// under an active dns_zones row for tenant `slug`. Being authoritative for a
// domain's DNS (NS delegated to us) IS proof of control, so this counts as
// verified — no separate challenge needed. Package-level (takes *sql.DB) so
// the mail op and the ingress resolver can both call it. Reads, never the
// hot-path snapshot.
func DomainCoveredByZone(ctx context.Context, db *sql.DB, slug, domain string, d registry.Dialect) (bool, error) {
	canon, ok := CanonicalizeHost(domain)
	if !ok || slug == "" {
		return false, nil
	}
	var one int
	err := db.QueryRowContext(ctx,
		orSQLite(d).Rebind(`SELECT 1 FROM dns_zones z
		   JOIN tenants t ON t.tenant_id = z.tenant_id
		  WHERE t.slug = ? AND t.revoked_at IS NULL
		    AND z.revoked_at IS NULL AND z.verified_at IS NOT NULL
		    AND (z.origin = ? OR ? LIKE '%.' || z.origin)
		  LIMIT 1`), slug, canon, canon).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// TenantForMailZone returns the slug of the tenant whose active zone covers
// `domain` (apex or subdomain), choosing the MOST SPECIFIC zone when several
// match (longest origin wins, so sub.example.com beats example.com). Used by
// the ingress resolver as a fallback when no tenant_hostnames row exists:
// "we serve DNS for it" ⟹ route mail to <slug>/_mail.
func TenantForMailZone(ctx context.Context, db *sql.DB, domain string, d registry.Dialect) (slug string, ok bool, err error) {
	canon, cok := CanonicalizeHost(domain)
	if !cok {
		return "", false, nil
	}
	err = db.QueryRowContext(ctx,
		orSQLite(d).Rebind(`SELECT t.slug FROM dns_zones z
		   JOIN tenants t ON t.tenant_id = z.tenant_id
		  WHERE z.revoked_at IS NULL AND t.revoked_at IS NULL
		    AND z.verified_at IS NOT NULL
		    AND (z.origin = ? OR ? LIKE '%.' || z.origin)
		  ORDER BY length(z.origin) DESC LIMIT 1`), canon, canon).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return slug, true, nil
}

// EnsureZoneHostnameTx makes sure (tenantID, stack) has an active
// routing hostname at `<StackLabel(stack)>.<origin>` — the SAME label
// the dns head synthesizes, so the resolved name and the HTTP/mail
// route match. Deterministic (no random suffix; the zone is
// tenant-scoped) and pre-verified (we are authoritative for the zone).
// Idempotent per (tenant, stack). Runs in the activation tx.
func EnsureZoneHostnameTx(ctx context.Context, tx *sql.Tx, tenantID, stack, origin, now string, d registry.Dialect) (string, error) {
	if tenantID == "" || stack == "" || origin == "" {
		return "", nil
	}
	d = orSQLite(d)
	label := StackLabel(stack)
	if label == "" {
		return "", nil
	}
	canon, ok := CanonicalizeHost(label + "." + strings.TrimSuffix(origin, "."))
	if !ok || !IsValidHostname(canon) {
		return "", errors.New("tenants: zone hostname invalid: " + label + "." + origin)
	}
	// Idempotency lookup for this (tenant, stack) managed zone-host; reused after
	// a mint conflict below.
	lookupExisting := func() (string, bool, error) {
		var h string
		err := tx.QueryRowContext(ctx,
			d.Rebind(`SELECT hostname FROM tenant_hostnames
			  WHERE tenant_id = ? AND stack = ? AND created_by = ? AND revoked_at IS NULL
			  LIMIT 1`), tenantID, stack, SystemZoneHostCreatedBy).Scan(&h)
		switch {
		case err == nil:
			return h, true, nil
		case errors.Is(err, sql.ErrNoRows):
			return "", false, nil
		default:
			return "", false, err
		}
	}
	// Adoption lookup: the active-hostname unique index is hostname-GLOBAL
	// (one active row per hostname, any creator), so a user-created binding
	// of the SAME deterministic host to the SAME (tenant, stack) — e.g. the
	// host was bound by hand before the zone was delegated — blocks the mint
	// forever while being functionally identical to its outcome. Adopt it
	// silently; only a host owned by a DIFFERENT tenant/stack is a real
	// conflict. (Observed: www.dripl.it warned on every activation.)
	lookupSameOwner := func() (bool, error) {
		var one int
		err := tx.QueryRowContext(ctx,
			d.Rebind(`SELECT 1 FROM tenant_hostnames
			  WHERE hostname = ? AND tenant_id = ? AND stack = ? AND revoked_at IS NULL
			  LIMIT 1`), canon, tenantID, stack).Scan(&one)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, sql.ErrNoRows):
			return false, nil
		default:
			return false, err
		}
	}
	if h, found, err := lookupExisting(); err != nil {
		return "", err
	} else if found {
		return h, nil
	}
	if found, err := lookupSameOwner(); err != nil {
		return "", err
	} else if found {
		return canon, nil
	}
	id := "thn_" + hxid.New().String()
	// SAVEPOINT-wrap the INSERT so a unique-violation doesn't poison the enclosing
	// activation tx on Postgres (on SQLite the savepoint is real but the recover
	// path never fires). The label is deterministic, so a conflict means the
	// identical host already exists.
	ierr := registry.RunInSavepoint(ctx, tx, "ezh", func() error {
		_, e := tx.ExecContext(ctx,
			d.Rebind(`INSERT INTO tenant_hostnames
			     (id, hostname, tenant_id, stack, created_at, created_by, verified_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`),
			id, canon, tenantID, stack, now, SystemZoneHostCreatedBy, now)
		return e
	})
	if ierr == nil {
		return canon, nil
	}
	if d.IsUniqueViolationGeneric(ierr) {
		// A concurrent activation of THIS (tenant, stack) may have just minted the
		// identical deterministic host — adopt it idempotently. Same for a
		// same-owner row that appeared since the pre-checks (lookupSameOwner).
		// If the hostname is instead owned by a different tenant/stack, both
		// lookups find nothing and we surface the conflict (the caller treats
		// a mint failure as non-fatal).
		if h, found, lerr := lookupExisting(); lerr != nil {
			return "", lerr
		} else if found {
			return h, nil
		}
		if found, lerr := lookupSameOwner(); lerr != nil {
			return "", lerr
		} else if found {
			return canon, nil
		}
	}
	return "", ierr
}

// DNSZone is a delegated zone. Timestamps are RFC3339 UTC text (empty
// RevokedAt = active).
type DNSZone struct {
	ID         string
	TenantID   string
	Origin     string
	MName      string
	RName      string
	Refresh    int
	Retry      int
	Expire     int
	Minimum    int
	DefaultTTL int
	Mode       string
	CreatedAt  string
	CreatedBy  string
	UpdatedAt  string
	RevokedAt  string
	// VerifiedAt gates whether the zone confers authority (0019). Empty/NULL =
	// pending (created with --dns-require-zone-verification on, awaiting an NS
	// check); set = verified. When the flag is off, CreateZoneTx stamps it at
	// creation so behavior is unchanged.
	VerifiedAt string
	// Per-domain DKIM material (0016), generated once on the control plane at
	// CreateZoneTx and fleet-synced on this row. Populated by GetZoneByIDTx
	// (for the producer); ListZones/LookupActiveZone leave them zero.
	DKIMSelector   string
	DKIMPrivatePEM string
	DKIMPublicB64  string
}

// DNSRecord is one override/extra record within a zone.
type DNSRecord struct {
	ID        string
	ZoneID    string
	Name      string
	Type      string
	TTL       sql.NullInt64
	Rdata     string
	CreatedAt string
	CreatedBy string
	UpdatedAt string
	RevokedAt string
}

// NewZoneID / NewRecordID mint the canonical prefixed surrogate IDs.
func NewZoneID() string   { return "dnz_" + hxid.New().String() }
func NewRecordID() string { return "dnr_" + hxid.New().String() }

// DNSSettings is the chassis-global synthesis infrastructure config —
// the nameservers customers delegate to, the edge A/AAAA target, and
// the mail exchanger. Singleton per chassis. List fields are stored
// comma-separated (matching the --dns-* flag convention).
type DNSSettings struct {
	Nameservers []string
	EdgeIPs     []string
	MXHost      string
	MXPriority  int
	SynthTTL    int
	UpdatedAt   string
	UpdatedBy   string
}

// rowQueryer is satisfied by both *sql.DB and *sql.Tx, so the settings
// read works on the live mirror (synthesis) or inside a tx (admin RMW).
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LoadDNSSettings reads the singleton dns_settings row. found=false when
// no row exists yet (first run) — callers then fall back to the boot
// `--dns-*` flag defaults.
func LoadDNSSettings(ctx context.Context, q rowQueryer, d registry.Dialect) (DNSSettings, bool, error) {
	var s DNSSettings
	var ns, edge string
	err := q.QueryRowContext(ctx,
		orSQLite(d).Rebind(`SELECT nameservers, edge_ips, mx_host, mx_priority, synth_ttl,
		        updated_at, COALESCE(updated_by, '')
		   FROM dns_settings WHERE singleton = 1`)).
		Scan(&ns, &edge, &s.MXHost, &s.MXPriority, &s.SynthTTL, &s.UpdatedAt, &s.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return DNSSettings{}, false, nil
	}
	if err != nil {
		return DNSSettings{}, false, err
	}
	s.Nameservers = splitCSV(ns)
	s.EdgeIPs = splitCSV(edge)
	return s, true, nil
}

// PutDNSSettingsTx upserts the singleton dns_settings row.
func PutDNSSettingsTx(ctx context.Context, tx *sql.Tx, s DNSSettings, d registry.Dialect) error {
	now := s.UpdatedAt
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	var updatedByArg any
	if s.UpdatedBy != "" {
		updatedByArg = s.UpdatedBy
	}
	ttl := s.SynthTTL
	if ttl <= 0 {
		ttl = 60
	}
	pri := s.MXPriority
	if pri < 0 {
		pri = 10
	}
	_, err := tx.ExecContext(ctx,
		orSQLite(d).Rebind(`INSERT INTO dns_settings
		     (singleton, nameservers, edge_ips, mx_host, mx_priority, synth_ttl, updated_at, updated_by)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(singleton) DO UPDATE SET
		     nameservers = excluded.nameservers,
		     edge_ips    = excluded.edge_ips,
		     mx_host     = excluded.mx_host,
		     mx_priority = excluded.mx_priority,
		     synth_ttl   = excluded.synth_ttl,
		     updated_at  = excluded.updated_at,
		     updated_by  = excluded.updated_by`),
		joinCSV(s.Nameservers), joinCSV(s.EdgeIPs), strings.TrimSpace(s.MXHost),
		pri, ttl, now, updatedByArg)
	return err
}

// splitCSV / joinCSV (de)serialize the comma-list settings columns,
// trimming blanks.
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func joinCSV(in []string) string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// ValidDNSRecordType reports whether t is a Phase-1/2 supported type.
func ValidDNSRecordType(t string) bool {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "NS", "A", "AAAA", "MX", "TXT", "CNAME":
		return true
	}
	return false
}

// ActiveRecordTypesAtNameTx returns the distinct active record types at
// (zoneID, name) in tx, name-normalized like CreateRecordTx ('' → '@').
// Feeds the write-path CNAME exclusivity check (RFC 1034 §3.6.2: a CNAME
// owner carries no other data, and at most one CNAME).
func ActiveRecordTypesAtNameTx(ctx context.Context, tx *sql.Tx, zoneID, name string, d registry.Dialect) ([]string, error) {
	if strings.TrimSpace(name) == "" {
		name = "@"
	}
	rows, err := tx.QueryContext(ctx,
		orSQLite(d).Rebind(`SELECT DISTINCT type FROM dns_records
		  WHERE zone_id = ? AND name = ? AND revoked_at IS NULL`),
		zoneID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// zoneSOADefaults fills sane SOA timers/TTL when a field is left zero.
func zoneSOADefaults(z *DNSZone) {
	if z.Refresh == 0 {
		z.Refresh = 7200
	}
	if z.Retry == 0 {
		z.Retry = 3600
	}
	if z.Expire == 0 {
		z.Expire = 1209600
	}
	if z.Minimum == 0 {
		z.Minimum = 90
	}
	if z.DefaultTTL == 0 {
		z.DefaultTTL = 60
	}
	if z.Mode == "" {
		z.Mode = "pattern"
	}
}

// CreateZoneTx inserts a delegated zone inside the caller's tx. The
// caller MUST supply z.ID (see CreateHostnameTx for the rationale). The
// origin is canonicalized; SOA timers default when zero. A second
// active zone for the same origin returns ErrZoneExists.
func (s *Store) CreateZoneTx(ctx context.Context, tx *sql.Tx, z DNSZone) error {
	if z.ID == "" {
		return errors.New("tenants: CreateZoneTx requires caller-supplied z.ID")
	}
	if z.TenantID == "" {
		return errors.New("tenants: empty tenant_id")
	}
	canon, ok := CanonicalizeHost(z.Origin)
	if !ok || !IsValidHostname(canon) {
		return errors.New("tenants: invalid zone origin")
	}
	if strings.TrimSpace(z.MName) == "" || strings.TrimSpace(z.RName) == "" {
		return errors.New("tenants: zone requires mname + rname (no nameservers configured?)")
	}
	if z.Mode != "" && z.Mode != "pattern" && z.Mode != "manual" {
		return errors.New("tenants: zone mode must be 'pattern' or 'manual'")
	}
	zoneSOADefaults(&z)
	// Mint a per-domain DKIM keypair once, here on the control plane, so the
	// public key the DNS head publishes and the private key any node signs
	// with come from the SAME material (fleet-synced on this row). A caller
	// may pre-supply the key (tests); otherwise generate. Skipped for manual
	// zones — DKIM is for the synthesized mail pattern.
	if z.DKIMPrivatePEM == "" && z.Mode != "manual" {
		priv, pub, gerr := GenerateDKIM()
		if gerr != nil {
			return gerr
		}
		z.DKIMSelector, z.DKIMPrivatePEM, z.DKIMPublicB64 = DKIMSelector, priv, pub
	}
	now := z.CreatedAt
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	var createdByArg any
	if z.CreatedBy != "" {
		createdByArg = z.CreatedBy
	}
	// verified_at (0019): the caller stamps it (handleCreateZone passes `now`
	// when --dns-require-zone-verification is off, "" when on → pending).
	var verifiedAtArg any
	if strings.TrimSpace(z.VerifiedAt) != "" {
		verifiedAtArg = z.VerifiedAt
	}
	_, err := tx.ExecContext(ctx,
		s.rb(`INSERT INTO dns_zones
		     (id, tenant_id, origin, mname, rname, refresh, retry, expire,
		      minimum, default_ttl, mode, created_at, created_by, updated_at,
		      dkim_selector, dkim_private_pem, dkim_public_b64, verified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		z.ID, z.TenantID, canon, z.MName, z.RName, z.Refresh, z.Retry, z.Expire,
		z.Minimum, z.DefaultTTL, z.Mode, now, createdByArg, now,
		z.DKIMSelector, z.DKIMPrivatePEM, z.DKIMPublicB64, verifiedAtArg)
	if err != nil {
		if s.dia().IsUniqueViolationGeneric(err) {
			return ErrZoneExists
		}
		return err
	}
	return nil
}

// ListZones returns a tenant's zones (active only unless includeRevoked).
func (s *Store) ListZones(ctx context.Context, tenantID string, includeRevoked bool) ([]DNSZone, error) {
	q := `SELECT id, tenant_id, origin, mname, rname, refresh, retry, expire,
	             minimum, default_ttl, mode, created_at, COALESCE(created_by, ''),
	             updated_at, COALESCE(revoked_at, ''), COALESCE(verified_at, '')
	        FROM dns_zones
	       WHERE tenant_id = ?`
	if !includeRevoked {
		q += ` AND revoked_at IS NULL`
	}
	q += ` ORDER BY origin`
	rows, err := s.query(ctx, q, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DNSZone
	for rows.Next() {
		var z DNSZone
		if err := rows.Scan(&z.ID, &z.TenantID, &z.Origin, &z.MName, &z.RName,
			&z.Refresh, &z.Retry, &z.Expire, &z.Minimum, &z.DefaultTTL, &z.Mode,
			&z.CreatedAt, &z.CreatedBy, &z.UpdatedAt, &z.RevokedAt, &z.VerifiedAt); err != nil {
			return nil, err
		}
		out = append(out, z)
	}
	return out, rows.Err()
}

// GetZoneByIDTx loads a single zone (active or revoked) by id from the tx, so a
// producer can read the fully-defaulted persisted row — SOA timers + timestamps
// filled by CreateZoneTx/RevokeZoneTx — to fleet-publish it. Returns ErrNotFound
// if absent.
func GetZoneByIDTx(ctx context.Context, tx *sql.Tx, id string, d registry.Dialect) (DNSZone, error) {
	var z DNSZone
	err := tx.QueryRowContext(ctx,
		orSQLite(d).Rebind(`SELECT id, tenant_id, origin, mname, rname, refresh, retry, expire,
		        minimum, default_ttl, mode, created_at, COALESCE(created_by, ''),
		        updated_at, COALESCE(revoked_at, ''), COALESCE(verified_at, ''),
		        dkim_selector, dkim_private_pem, dkim_public_b64
		   FROM dns_zones
		  WHERE id = ?`), id).Scan(&z.ID, &z.TenantID, &z.Origin, &z.MName, &z.RName,
		&z.Refresh, &z.Retry, &z.Expire, &z.Minimum, &z.DefaultTTL, &z.Mode,
		&z.CreatedAt, &z.CreatedBy, &z.UpdatedAt, &z.RevokedAt, &z.VerifiedAt,
		&z.DKIMSelector, &z.DKIMPrivatePEM, &z.DKIMPublicB64)
	if errors.Is(err, sql.ErrNoRows) {
		return DNSZone{}, ErrNotFound
	}
	if err != nil {
		return DNSZone{}, err
	}
	return z, nil
}

// LookupActiveZone returns the active zone for (tenantID, origin), or
// ErrNotFound. Used to authorize record writes against the caller's zone.
func (s *Store) LookupActiveZone(ctx context.Context, tenantID, origin string) (DNSZone, error) {
	canon, ok := CanonicalizeHost(origin)
	if !ok {
		return DNSZone{}, ErrNotFound
	}
	var z DNSZone
	err := s.queryRow(ctx,
		`SELECT id, tenant_id, origin, mname, rname, refresh, retry, expire,
		        minimum, default_ttl, mode, created_at, COALESCE(created_by, ''),
		        updated_at, COALESCE(revoked_at, '')
		   FROM dns_zones
		  WHERE tenant_id = ? AND origin = ? AND revoked_at IS NULL`,
		tenantID, canon).Scan(&z.ID, &z.TenantID, &z.Origin, &z.MName, &z.RName,
		&z.Refresh, &z.Retry, &z.Expire, &z.Minimum, &z.DefaultTTL, &z.Mode,
		&z.CreatedAt, &z.CreatedBy, &z.UpdatedAt, &z.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DNSZone{}, ErrNotFound
	}
	if err != nil {
		return DNSZone{}, err
	}
	return z, nil
}

// SetZoneVerifiedTx stamps verified_at (and bumps updated_at, so the synthesized
// SOA serial advances) on a tenant's active zone — flipping a PENDING zone live
// once its NS delegation is confirmed. Idempotent (re-verify refreshes it).
// ErrNotFound if no active zone for (tenantID, origin).
func SetZoneVerifiedTx(ctx context.Context, tx *sql.Tx, tenantID, origin, now string, d registry.Dialect) error {
	canon, ok := CanonicalizeHost(origin)
	if !ok {
		return ErrNotFound
	}
	res, err := tx.ExecContext(ctx,
		orSQLite(d).Rebind(`UPDATE dns_zones SET verified_at = ?, updated_at = ?
		  WHERE tenant_id = ? AND origin = ? AND revoked_at IS NULL`),
		now, now, tenantID, canon)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeZoneTx soft-revokes a tenant's active zone by origin. Lenient:
// an already-absent zone returns ErrNotFound. Records under it stop
// serving once the zone is gone from the snapshot (zones are read
// WHERE revoked_at IS NULL).
func (s *Store) RevokeZoneTx(ctx context.Context, tx *sql.Tx, tenantID, origin string) (string, error) {
	canon, ok := CanonicalizeHost(origin)
	if !ok {
		return "", ErrNotFound
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx,
		s.rb(`UPDATE dns_zones SET revoked_at = ?
		  WHERE tenant_id = ? AND origin = ? AND revoked_at IS NULL`),
		now, tenantID, canon)
	if err != nil {
		return canon, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return canon, ErrNotFound
	}
	return canon, nil
}

// CreateRecordTx inserts an override/extra record under a zone. Caller
// supplies r.ID and a validated, zone-owned r.ZoneID.
func (s *Store) CreateRecordTx(ctx context.Context, tx *sql.Tx, r DNSRecord) error {
	if r.ID == "" || r.ZoneID == "" {
		return errors.New("tenants: CreateRecordTx requires r.ID + r.ZoneID")
	}
	if !ValidDNSRecordType(r.Type) {
		return errors.New("tenants: invalid record type")
	}
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = "@"
	}
	if strings.TrimSpace(r.Rdata) == "" {
		return errors.New("tenants: empty rdata")
	}
	now := r.CreatedAt
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	var createdByArg any
	if r.CreatedBy != "" {
		createdByArg = r.CreatedBy
	}
	_, err := tx.ExecContext(ctx,
		s.rb(`INSERT INTO dns_records
		     (id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		r.ID, r.ZoneID, name, strings.ToUpper(r.Type), r.TTL, r.Rdata,
		now, createdByArg, now)
	return err
}

// ListRecords returns the active records for a zone.
func (s *Store) ListRecords(ctx context.Context, zoneID string) ([]DNSRecord, error) {
	rows, err := s.query(ctx,
		`SELECT id, zone_id, name, type, ttl, rdata, created_at,
		        COALESCE(created_by, ''), updated_at, COALESCE(revoked_at, '')
		   FROM dns_records
		  WHERE zone_id = ? AND revoked_at IS NULL
		  ORDER BY name, type`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DNSRecord
	for rows.Next() {
		var r DNSRecord
		if err := rows.Scan(&r.ID, &r.ZoneID, &r.Name, &r.Type, &r.TTL, &r.Rdata,
			&r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeRecordTx soft-revokes active records matching (zoneID, name,
// type). Returns the revoked row ids so the caller can fleet-publish
// the flipped rows; ErrNotFound when none matched.
func (s *Store) RevokeRecordTx(ctx context.Context, tx *sql.Tx, zoneID, name, rtype string) ([]string, error) {
	if name == "" {
		name = "@"
	}
	// Ids first, then update by id: (zone, name, type) is not unique
	// (multi-rdata MX/TXT sets), and the id set pins exactly the rows
	// this call flipped — a timestamp match could catch a neighbour.
	rows, err := tx.QueryContext(ctx,
		s.rb(`SELECT id FROM dns_records
		  WHERE zone_id = ? AND name = ? AND type = ? AND revoked_at IS NULL`),
		zoneID, name, strings.ToUpper(rtype))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, ErrNotFound
	}
	now := time.Now().UTC().Format(time.RFC3339)
	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	ph := make([]string, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx,
		s.rb(`UPDATE dns_records SET revoked_at = ? WHERE id IN (`+strings.Join(ph, ", ")+`)`),
		args...); err != nil {
		return nil, err
	}
	return ids, nil
}

// GetRecordByIDTx loads a single record (active or revoked) by id from the tx,
// so a producer can read the fully-normalized persisted row — '@' apex,
// uppercased type, timestamp defaults filled by CreateRecordTx — to
// fleet-publish it. Mirrors GetZoneByIDTx. Returns ErrNotFound if absent.
func GetRecordByIDTx(ctx context.Context, tx *sql.Tx, id string, d registry.Dialect) (DNSRecord, error) {
	var r DNSRecord
	err := tx.QueryRowContext(ctx,
		orSQLite(d).Rebind(`SELECT id, zone_id, name, type, ttl, rdata, created_at,
		        COALESCE(created_by, ''), updated_at, COALESCE(revoked_at, '')
		   FROM dns_records
		  WHERE id = ?`), id).Scan(&r.ID, &r.ZoneID, &r.Name, &r.Type, &r.TTL,
		&r.Rdata, &r.CreatedAt, &r.CreatedBy, &r.UpdatedAt, &r.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DNSRecord{}, ErrNotFound
	}
	if err != nil {
		return DNSRecord{}, err
	}
	return r, nil
}
