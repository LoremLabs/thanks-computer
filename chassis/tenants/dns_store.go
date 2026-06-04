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
// zone origin (lexically first when several exist), or ok=false. Read
// inside the activation tx so the minted routing host is consistent
// with the same transaction's view of the zone set.
func ActivePatternZoneOriginTx(ctx context.Context, tx *sql.Tx, tenantID string) (string, bool, error) {
	var origin string
	err := tx.QueryRowContext(ctx,
		`SELECT origin FROM dns_zones
		  WHERE tenant_id = ? AND mode = 'pattern' AND revoked_at IS NULL
		  ORDER BY origin LIMIT 1`, tenantID).Scan(&origin)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return origin, origin != "", nil
}

// EnsureZoneHostnameTx makes sure (tenantID, stack) has an active
// routing hostname at `<StackLabel(stack)>.<origin>` — the SAME label
// the dns head synthesizes, so the resolved name and the HTTP/mail
// route match. Deterministic (no random suffix; the zone is
// tenant-scoped) and pre-verified (we are authoritative for the zone).
// Idempotent per (tenant, stack). Runs in the activation tx.
func EnsureZoneHostnameTx(ctx context.Context, tx *sql.Tx, tenantID, stack, origin, now string) (string, error) {
	if tenantID == "" || stack == "" || origin == "" {
		return "", nil
	}
	label := StackLabel(stack)
	if label == "" {
		return "", nil
	}
	canon, ok := CanonicalizeHost(label + "." + strings.TrimSuffix(origin, "."))
	if !ok || !IsValidHostname(canon) {
		return "", errors.New("tenants: zone hostname invalid: " + label + "." + origin)
	}
	var existing string
	err := tx.QueryRowContext(ctx,
		`SELECT hostname FROM tenant_hostnames
		  WHERE tenant_id = ? AND stack = ? AND created_by = ? AND revoked_at IS NULL
		  LIMIT 1`, tenantID, stack, SystemZoneHostCreatedBy).Scan(&existing)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	id := "thn_" + hxid.New().String()
	if _, ierr := tx.ExecContext(ctx,
		`INSERT INTO tenant_hostnames
		     (id, hostname, tenant_id, stack, created_at, created_by, verified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, canon, tenantID, stack, now, SystemZoneHostCreatedBy, now); ierr != nil {
		return "", ierr
	}
	return canon, nil
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
func LoadDNSSettings(ctx context.Context, q rowQueryer) (DNSSettings, bool, error) {
	var s DNSSettings
	var ns, edge string
	err := q.QueryRowContext(ctx,
		`SELECT nameservers, edge_ips, mx_host, mx_priority, synth_ttl,
		        updated_at, COALESCE(updated_by, '')
		   FROM dns_settings WHERE singleton = 1`).
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
func PutDNSSettingsTx(ctx context.Context, tx *sql.Tx, s DNSSettings) error {
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
		`INSERT INTO dns_settings
		     (singleton, nameservers, edge_ips, mx_host, mx_priority, synth_ttl, updated_at, updated_by)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(singleton) DO UPDATE SET
		     nameservers = excluded.nameservers,
		     edge_ips    = excluded.edge_ips,
		     mx_host     = excluded.mx_host,
		     mx_priority = excluded.mx_priority,
		     synth_ttl   = excluded.synth_ttl,
		     updated_at  = excluded.updated_at,
		     updated_by  = excluded.updated_by`,
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
	case "NS", "A", "AAAA", "MX", "TXT":
		return true
	}
	return false
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
	now := z.CreatedAt
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	var createdByArg any
	if z.CreatedBy != "" {
		createdByArg = z.CreatedBy
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO dns_zones
		     (id, tenant_id, origin, mname, rname, refresh, retry, expire,
		      minimum, default_ttl, mode, created_at, created_by, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		z.ID, z.TenantID, canon, z.MName, z.RName, z.Refresh, z.Retry, z.Expire,
		z.Minimum, z.DefaultTTL, z.Mode, now, createdByArg, now)
	if err != nil {
		if isUniqueViolation(err) {
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
	             updated_at, COALESCE(revoked_at, '')
	        FROM dns_zones
	       WHERE tenant_id = ?`
	if !includeRevoked {
		q += ` AND revoked_at IS NULL`
	}
	q += ` ORDER BY origin`
	rows, err := s.DB.QueryContext(ctx, q, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DNSZone
	for rows.Next() {
		var z DNSZone
		if err := rows.Scan(&z.ID, &z.TenantID, &z.Origin, &z.MName, &z.RName,
			&z.Refresh, &z.Retry, &z.Expire, &z.Minimum, &z.DefaultTTL, &z.Mode,
			&z.CreatedAt, &z.CreatedBy, &z.UpdatedAt, &z.RevokedAt); err != nil {
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
func GetZoneByIDTx(ctx context.Context, tx *sql.Tx, id string) (DNSZone, error) {
	var z DNSZone
	err := tx.QueryRowContext(ctx,
		`SELECT id, tenant_id, origin, mname, rname, refresh, retry, expire,
		        minimum, default_ttl, mode, created_at, COALESCE(created_by, ''),
		        updated_at, COALESCE(revoked_at, '')
		   FROM dns_zones
		  WHERE id = ?`, id).Scan(&z.ID, &z.TenantID, &z.Origin, &z.MName, &z.RName,
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

// LookupActiveZone returns the active zone for (tenantID, origin), or
// ErrNotFound. Used to authorize record writes against the caller's zone.
func (s *Store) LookupActiveZone(ctx context.Context, tenantID, origin string) (DNSZone, error) {
	canon, ok := CanonicalizeHost(origin)
	if !ok {
		return DNSZone{}, ErrNotFound
	}
	var z DNSZone
	err := s.DB.QueryRowContext(ctx,
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
		`UPDATE dns_zones SET revoked_at = ?
		  WHERE tenant_id = ? AND origin = ? AND revoked_at IS NULL`,
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
		`INSERT INTO dns_records
		     (id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.ZoneID, name, strings.ToUpper(r.Type), r.TTL, r.Rdata,
		now, createdByArg, now)
	return err
}

// ListRecords returns the active records for a zone.
func (s *Store) ListRecords(ctx context.Context, zoneID string) ([]DNSRecord, error) {
	rows, err := s.DB.QueryContext(ctx,
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
// type). Returns ErrNotFound when none matched.
func (s *Store) RevokeRecordTx(ctx context.Context, tx *sql.Tx, zoneID, name, rtype string) error {
	if name == "" {
		name = "@"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx,
		`UPDATE dns_records SET revoked_at = ?
		  WHERE zone_id = ? AND name = ? AND type = ? AND revoked_at IS NULL`,
		now, zoneID, name, strings.ToUpper(rtype))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
