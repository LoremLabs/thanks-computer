package secrets

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Sentinel errors.
var (
	// ErrSecretNotFound signals no active row for (tenant, scope, name).
	ErrSecretNotFound = errors.New("secrets: not found")
	// ErrSecretExists signals a duplicate name within (tenant, stack).
	// Admin handlers translate to HTTP 409.
	ErrSecretExists = errors.New("secrets: name already in use for this scope")
	// ErrInvalidName signals a name that violates the shape rule.
	// Admin handlers translate to HTTP 400.
	ErrInvalidName = errors.New("secrets: invalid name (must match [A-Za-z][A-Za-z0-9_]*)")
)

// nameRE pins the on-disk shape of `name`: must start with a letter,
// then any of [A-Za-z 0-9 _]. Case is NOT constrained — UPPER_SNAKE is
// the convention but not enforced (the name is a quoted-string value
// in txcl WITH clauses, opaque to the parser, and matched
// case-sensitively in the store). Keeping it an identifier (no spaces,
// dashes, or slashes) keeps the `/secrets/{name}` route and CLI/UI
// rendering predictable. Length capped to keep the unique-index key
// small.
var nameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)

// SecretMetadata is the metadata view of a tenant_secrets row
// joined with its active tenant_secret_versions row. NEVER carries
// cleartext — callers materialize via the resolver path.
type SecretMetadata struct {
	SecretID      string
	TenantID      string
	Stack         *string // nil = tenant-wide
	Name          string
	Description   string
	CreatedAt     time.Time
	CreatedBy     string
	LastRotatedAt *time.Time
	RevokedAt     *time.Time // typically nil for rows the Store returns
	KeyVersion    int
	VersionNo     int // active version number (latest non-revoked)
}

// Store is the thin façade over tenant_secrets + tenant_secret_versions.
// Plain *sql.DB (no dialect seam) matches chassis/tenants/store.go:
// runtime tables stay SQLite-only per-machine. If/when the runtime
// store moves to Postgres for HA, this and tenants.Store adopt the
// dialect together (so the schema-level decision is uniform).
type Store struct {
	DB *sql.DB
	MK MasterKeyProvider

	// syncer, when non-nil, fleet-publishes every write so other nodes'
	// local copies converge. Installed via SetSyncer during admin wiring;
	// nil = single-node (no producer work). See sync.go.
	syncer Syncer

	// now is a clock seam for tests. Defaults to time.Now.UTC.
	now func() time.Time
}

// NewStore builds a Store against the given runtime *sql.DB and
// MasterKeyProvider. The MK is required — a nil MK means "feature
// off"; callers wiring boot logic (PR 2 step in chassis/app/app.go)
// should construct the Store only when SecretMasterKeyPath is set.
func NewStore(db *sql.DB, mk MasterKeyProvider) *Store {
	return &Store{DB: db, MK: mk, now: func() time.Time { return time.Now().UTC() }}
}

// CreateSecret stores a new value supplied by the caller (operator).
// stack=nil → tenant-wide; stack=non-nil → scoped to that stack.
// Returns ErrSecretExists if (tenant_id, stack, name) is already
// active (the COALESCE-bound unique index catches NULL-stack dupes
// too — see db/schema/sqlite/runtime/0008_tenant_secrets.sql).
func (s *Store) CreateSecret(ctx context.Context,
	tenantID string, stack *string, name, description, createdBy string,
	value []byte,
) (*SecretMetadata, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if s.MK == nil {
		return nil, fmt.Errorf("secrets: no master key configured")
	}
	if len(value) == 0 {
		return nil, fmt.Errorf("secrets: empty value")
	}

	secretID := "sec_" + hxid.NewTimeSort().String()
	versionID := "sec_v_" + hxid.NewTimeSort().String()
	const versionNo = 1
	keyVer := s.MK.Version()
	createdAt := s.now()

	es, err := Encrypt(s.MK, value,
		outerAAD(tenantID, secretID, versionNo, name, keyVer),
		innerAAD(tenantID, secretID, versionNo, keyVer))
	if err != nil {
		return nil, fmt.Errorf("secrets: encrypt: %w", err)
	}

	meta := &SecretMetadata{
		SecretID:    secretID,
		TenantID:    tenantID,
		Stack:       copyStringPtr(stack),
		Name:        name,
		Description: description,
		CreatedAt:   createdAt,
		CreatedBy:   createdBy,
		KeyVersion:  keyVer,
		VersionNo:   versionNo,
	}

	// Fleet-publish: upload the row artifacts (pre-tx) and get the in-tx
	// outbox closure so the secret and its propagation commit atomically.
	// nil when single-node. version row first, then parent (see Syncer).
	inTx, err := s.publishUpsert(ctx, tenantID,
		versionRowMap(versionID, secretID, versionNo, es, createdAt),
		parentRowMap(meta))
	if err != nil {
		return nil, fmt.Errorf("secrets: fleet publish: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO tenant_secrets
			(secret_id, tenant_id, stack, name, description, created_at, created_by, key_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		secretID, tenantID, nilIfEmptyStack(stack), name, description,
		createdAt.Format(time.RFC3339), createdBy, keyVer)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSecretExists
		}
		return nil, fmt.Errorf("secrets: insert tenant_secrets: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO tenant_secret_versions
			(version_id, secret_id, version_no, nonce, ciphertext, wrapped_dek, dek_nonce, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		versionID, secretID, versionNo, es.Nonce, es.Ciphertext, es.WrappedDEK, es.DEKNonce,
		createdAt.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("secrets: insert tenant_secret_versions: %w", err)
	}

	if inTx != nil {
		if err := inTx(tx); err != nil {
			return nil, fmt.Errorf("secrets: fleet outbox: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("secrets: commit: %w", err)
	}

	return meta, nil
}

// GenerateSecret mints byteLen random bytes (raw — caller may choose
// to base64/hex-render for the operator on the way out) and stores
// them. Returns cleartext exactly once for the caller to surface;
// caller is responsible for zeroing.
func (s *Store) GenerateSecret(ctx context.Context,
	tenantID string, stack *string, name, description, createdBy string,
	byteLen int,
) (cleartext []byte, meta *SecretMetadata, err error) {
	if byteLen <= 0 || byteLen > 4096 {
		return nil, nil, fmt.Errorf("secrets: byteLen out of range (1..4096)")
	}
	cleartext = make([]byte, byteLen)
	if _, err = io.ReadFull(rand.Reader, cleartext); err != nil {
		return nil, nil, fmt.Errorf("secrets: mint value: %w", err)
	}
	meta, err = s.CreateSecret(ctx, tenantID, stack, name, description, createdBy, cleartext)
	if err != nil {
		Zero(cleartext)
		return nil, nil, err
	}
	return cleartext, meta, nil
}

// LookupSecretMetadata returns metadata for one secret in an exact
// scope. stack=nil → look up the tenant-wide row; stack=non-nil →
// look up exactly that stack-scoped row. No fallback (that's
// MaterializeSecretForOp's job). Returns ErrSecretNotFound if absent.
func (s *Store) LookupSecretMetadata(ctx context.Context,
	tenantID string, stack *string, name string,
) (*SecretMetadata, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	return s.lookupMetadataExact(ctx, tenantID, stack, name)
}

// ListSecrets returns metadata for all active secrets in a tenant,
// both tenant-wide and stack-scoped. Ordered by name then stack
// (NULL stack first via COALESCE).
func (s *Store) ListSecrets(ctx context.Context, tenantID string) ([]*SecretMetadata, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT s.secret_id, s.tenant_id, s.stack, s.name, COALESCE(s.description,''),
		       s.created_at, COALESCE(s.created_by,''), s.last_rotated_at, s.revoked_at, s.key_version,
		       COALESCE((SELECT MAX(version_no) FROM tenant_secret_versions v
		                 WHERE v.secret_id = s.secret_id AND v.revoked_at IS NULL), 0) AS version_no
		FROM tenant_secrets s
		WHERE s.tenant_id = ? AND s.revoked_at IS NULL
		ORDER BY s.name, COALESCE(s.stack, '')`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("secrets: list: %w", err)
	}
	defer rows.Close()

	var out []*SecretMetadata
	for rows.Next() {
		m, err := scanMetadata(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListForResync returns the fleet-sync row payloads for every ACTIVE secret
// in the tenant (parent row + its active version row). Cleartext is never
// produced — the encrypted blob columns travel as-is. Producer-side only:
// the admin resync path re-emits these so lagging/new nodes converge.
func (s *Store) ListForResync(ctx context.Context, tenantID string) ([]ResyncRow, error) {
	metas, err := s.ListSecrets(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]ResyncRow, 0, len(metas))
	for _, m := range metas {
		var versionID, createdAtStr string
		var nonce, ciphertext, wrappedDEK, dekNonce []byte
		err := s.DB.QueryRowContext(ctx, `
			SELECT version_id, nonce, ciphertext, wrapped_dek, dek_nonce, created_at
			FROM tenant_secret_versions
			WHERE secret_id = ? AND version_no = ? AND revoked_at IS NULL`,
			m.SecretID, m.VersionNo).Scan(&versionID, &nonce, &ciphertext, &wrappedDEK, &dekNonce, &createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("secrets: resync load version %s v%d: %w", m.SecretID, m.VersionNo, err)
		}
		es := &EncryptedSecret{
			Nonce: nonce, Ciphertext: ciphertext,
			WrappedDEK: wrappedDEK, DEKNonce: dekNonce, KeyVersion: m.KeyVersion,
		}
		createdAt, _ := time.Parse(time.RFC3339, createdAtStr)
		out = append(out, ResyncRow{
			SecretID:   m.SecretID,
			VersionID:  versionID,
			ParentRow:  parentRowMap(m),
			VersionRow: versionRowMap(versionID, m.SecretID, m.VersionNo, es, createdAt),
		})
	}
	return out, nil
}

// MaterializeSecretForOp is the runtime resolution path. Performs
// stack-scoped → tenant-wide fallback per internal docs/todo-secret-store.md §2:
//
//  1. Try (tenant_id, stack, name) — stack-scoped wins if present.
//  2. Fall back to (tenant_id, stack IS NULL, name) — tenant-wide.
//  3. Otherwise ErrSecretNotFound.
//
// Returns cleartext and metadata of the row that won. Caller is
// responsible for zeroing the cleartext (SecretBag.Zero does this
// automatically once PR 3 wires the request-scoped bag).
//
// Pass stack="" for "I have no current stack; only look at
// tenant-wide" (admin paths, internal hooks).
func (s *Store) MaterializeSecretForOp(ctx context.Context,
	tenantID, stack, name string,
) ([]byte, *SecretMetadata, error) {
	if err := validateName(name); err != nil {
		return nil, nil, err
	}
	if s.MK == nil {
		return nil, nil, fmt.Errorf("secrets: no master key configured")
	}

	// 1. Try stack-scoped first if a stack is provided.
	if stack != "" {
		if meta, err := s.lookupMetadataExact(ctx, tenantID, &stack, name); err == nil {
			pt, derr := s.decryptActive(ctx, meta)
			if derr != nil {
				return nil, nil, derr
			}
			return pt, meta, nil
		} else if !errors.Is(err, ErrSecretNotFound) {
			return nil, nil, err
		}
		// fall through to tenant-wide
	}

	// 2. Tenant-wide fallback.
	meta, err := s.lookupMetadataExact(ctx, tenantID, nil, name)
	if err != nil {
		return nil, nil, err
	}
	pt, err := s.decryptActive(ctx, meta)
	if err != nil {
		return nil, nil, err
	}
	return pt, meta, nil
}

// RevealSecretValue is the break-glass Go hook for high-trust paths.
// Not wired to any v1 HTTP endpoint or CLI (per design §5). Exact-
// scope match; no fallback. Caller is responsible for zeroing.
func (s *Store) RevealSecretValue(ctx context.Context,
	tenantID string, stack *string, name string,
) ([]byte, *SecretMetadata, error) {
	if err := validateName(name); err != nil {
		return nil, nil, err
	}
	if s.MK == nil {
		return nil, nil, fmt.Errorf("secrets: no master key configured")
	}
	meta, err := s.lookupMetadataExact(ctx, tenantID, stack, name)
	if err != nil {
		return nil, nil, err
	}
	pt, err := s.decryptActive(ctx, meta)
	if err != nil {
		return nil, nil, err
	}
	return pt, meta, nil
}

// UpdateSecretDescription mutates only the description field. Name
// is immutable (design §1.7); this is the only "update" path.
func (s *Store) UpdateSecretDescription(ctx context.Context,
	tenantID string, stack *string, name, newDescription string,
) (*SecretMetadata, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	// Load the full active row first: the fleet REPLACE artifact needs every
	// column (a description-only patch would otherwise blank created_at etc.).
	meta, err := s.lookupMetadataExact(ctx, tenantID, stack, name)
	if err != nil {
		return nil, err // ErrSecretNotFound bubbles
	}
	meta.Description = newDescription

	// Parent-only change → nil version row.
	inTx, err := s.publishUpsert(ctx, tenantID, nil, parentRowMap(meta))
	if err != nil {
		return nil, fmt.Errorf("secrets: fleet publish: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE tenant_secrets
		SET description = ?
		WHERE secret_id = ? AND revoked_at IS NULL`,
		newDescription, meta.SecretID)
	if err != nil {
		return nil, fmt.Errorf("secrets: update description: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, ErrSecretNotFound
	}

	if inTx != nil {
		if err := inTx(tx); err != nil {
			return nil, fmt.Errorf("secrets: fleet outbox: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("secrets: commit: %w", err)
	}
	return s.lookupMetadataExact(ctx, tenantID, stack, name)
}

// RotateSecret writes a new version row under the same secret_id
// using an operator-supplied value. Old versions are kept for audit.
// Updates last_rotated_at and key_version on the parent row.
func (s *Store) RotateSecret(ctx context.Context,
	tenantID string, stack *string, name string, newValue []byte,
) (*SecretMetadata, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if s.MK == nil {
		return nil, fmt.Errorf("secrets: no master key configured")
	}
	if len(newValue) == 0 {
		return nil, fmt.Errorf("secrets: empty value")
	}

	// Load the full current row + the global max version BEFORE the write tx
	// so the fleet artifact uploads pre-tx. The secret store is the only
	// producer with a version counter; rotations are rare, serialized
	// operator actions, so reading max(version_no) here (vs inside the tx) is
	// safe. The parent REPLACE artifact needs every column, so we load full
	// metadata rather than the old (secret_id, key_version) projection.
	meta0, err := s.lookupMetadataExact(ctx, tenantID, stack, name)
	if err != nil {
		return nil, err // ErrSecretNotFound bubbles
	}
	secretID := meta0.SecretID

	// New version number = max(version_no) + 1 (over ALL versions, incl.
	// revoked, so a number is never reused).
	var maxVer sql.NullInt64
	if err := s.DB.QueryRowContext(ctx,
		`SELECT MAX(version_no) FROM tenant_secret_versions WHERE secret_id = ?`,
		secretID).Scan(&maxVer); err != nil {
		return nil, fmt.Errorf("secrets: max version: %w", err)
	}
	versionNo := int(maxVer.Int64) + 1
	rotatedAt := s.now()

	// Use the CURRENT MK version (in case MK was rotated since the row was
	// first encrypted). PR 2 always == previous because online MK rotation
	// is Phase 2.
	newKeyVer := s.MK.Version()
	es, err := Encrypt(s.MK, newValue,
		outerAAD(tenantID, secretID, versionNo, name, newKeyVer),
		innerAAD(tenantID, secretID, versionNo, newKeyVer))
	if err != nil {
		return nil, fmt.Errorf("secrets: encrypt rotated: %w", err)
	}
	versionID := "sec_v_" + hxid.NewTimeSort().String()

	// Post-rotate parent row (full row for the consumer's INSERT OR REPLACE).
	newMeta := *meta0
	newMeta.VersionNo = versionNo
	newMeta.KeyVersion = newKeyVer
	newMeta.LastRotatedAt = &rotatedAt
	inTx, err := s.publishUpsert(ctx, tenantID,
		versionRowMap(versionID, secretID, versionNo, es, rotatedAt),
		parentRowMap(&newMeta))
	if err != nil {
		return nil, fmt.Errorf("secrets: fleet publish: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO tenant_secret_versions
			(version_id, secret_id, version_no, nonce, ciphertext, wrapped_dek, dek_nonce, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		versionID, secretID, versionNo, es.Nonce, es.Ciphertext, es.WrappedDEK, es.DEKNonce,
		rotatedAt.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("secrets: insert rotated version: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE tenant_secrets
		SET last_rotated_at = ?, key_version = ?
		WHERE secret_id = ?`,
		rotatedAt.Format(time.RFC3339), newKeyVer, secretID)
	if err != nil {
		return nil, fmt.Errorf("secrets: bump rotated_at: %w", err)
	}

	if inTx != nil {
		if err := inTx(tx); err != nil {
			return nil, fmt.Errorf("secrets: fleet outbox: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("secrets: commit rotate: %w", err)
	}

	return s.lookupMetadataExact(ctx, tenantID, stack, name)
}

// RotateSecretGenerated is RotateSecret with chassis-minted bytes.
// Returns cleartext exactly once.
func (s *Store) RotateSecretGenerated(ctx context.Context,
	tenantID string, stack *string, name string, byteLen int,
) ([]byte, *SecretMetadata, error) {
	if byteLen <= 0 || byteLen > 4096 {
		return nil, nil, fmt.Errorf("secrets: byteLen out of range (1..4096)")
	}
	cleartext := make([]byte, byteLen)
	if _, err := io.ReadFull(rand.Reader, cleartext); err != nil {
		return nil, nil, fmt.Errorf("secrets: mint value: %w", err)
	}
	meta, err := s.RotateSecret(ctx, tenantID, stack, name, cleartext)
	if err != nil {
		Zero(cleartext)
		return nil, nil, err
	}
	return cleartext, meta, nil
}

// RevokeSecret soft-deletes by setting revoked_at on the parent row.
// Version rows are preserved for audit history. Once revoked, the
// COALESCE-bound unique index frees the (tenant_id, stack, name)
// slot for re-creation.
func (s *Store) RevokeSecret(ctx context.Context,
	tenantID string, stack *string, name string,
) error {
	if err := validateName(name); err != nil {
		return err
	}
	// Load the full active row first: the fleet REPLACE artifact needs every
	// column (with revoked_at now set), and this confirms existence.
	meta, err := s.lookupMetadataExact(ctx, tenantID, stack, name)
	if err != nil {
		return err // ErrSecretNotFound bubbles
	}
	revokedAt := s.now()
	meta.RevokedAt = &revokedAt

	inTx, err := s.publishRevoke(ctx, tenantID, parentRowMap(meta))
	if err != nil {
		return fmt.Errorf("secrets: fleet publish: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("secrets: begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE tenant_secrets
		SET revoked_at = ?
		WHERE secret_id = ? AND revoked_at IS NULL`,
		revokedAt.Format(time.RFC3339), meta.SecretID)
	if err != nil {
		return fmt.Errorf("secrets: revoke: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrSecretNotFound
	}

	if inTx != nil {
		if err := inTx(tx); err != nil {
			return fmt.Errorf("secrets: fleet outbox: %w", err)
		}
	}

	return tx.Commit()
}

// --- helpers ---

// lookupMetadataExact reads one active row by (tenant_id, stack, name).
// Used by both admin reads and the materialization path.
func (s *Store) lookupMetadataExact(ctx context.Context,
	tenantID string, stack *string, name string,
) (*SecretMetadata, error) {
	row := s.DB.QueryRowContext(ctx, `
		SELECT s.secret_id, s.tenant_id, s.stack, s.name, COALESCE(s.description,''),
		       s.created_at, COALESCE(s.created_by,''), s.last_rotated_at, s.revoked_at, s.key_version,
		       COALESCE((SELECT MAX(version_no) FROM tenant_secret_versions v
		                 WHERE v.secret_id = s.secret_id AND v.revoked_at IS NULL), 0) AS version_no
		FROM tenant_secrets s
		WHERE s.tenant_id = ? AND COALESCE(s.stack,'') = COALESCE(?,'')
		      AND s.name = ? AND s.revoked_at IS NULL`,
		tenantID, nilIfEmptyStack(stack), name)
	m, err := scanMetadata(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSecretNotFound
	}
	return m, err
}

// decryptActive loads the active version row for meta and decrypts.
func (s *Store) decryptActive(ctx context.Context, meta *SecretMetadata) ([]byte, error) {
	var nonce, ciphertext, wrappedDEK, dekNonce []byte
	err := s.DB.QueryRowContext(ctx, `
		SELECT nonce, ciphertext, wrapped_dek, dek_nonce
		FROM tenant_secret_versions
		WHERE secret_id = ? AND version_no = ? AND revoked_at IS NULL`,
		meta.SecretID, meta.VersionNo).Scan(&nonce, &ciphertext, &wrappedDEK, &dekNonce)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("secrets: active version row missing for %s v%d", meta.SecretID, meta.VersionNo)
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: load version: %w", err)
	}
	es := &EncryptedSecret{
		Nonce:      nonce,
		Ciphertext: ciphertext,
		WrappedDEK: wrappedDEK,
		DEKNonce:   dekNonce,
		KeyVersion: meta.KeyVersion,
	}
	return Decrypt(s.MK, es,
		outerAAD(meta.TenantID, meta.SecretID, meta.VersionNo, meta.Name, meta.KeyVersion),
		innerAAD(meta.TenantID, meta.SecretID, meta.VersionNo, meta.KeyVersion))
}

// outerAAD binds the secret ciphertext to row identity. Layout per
// design §3. Any change to this layout is a breaking ciphertext-
// format change; bump key_version and migrate.
func outerAAD(tenantID, secretID string, versionNo int, name string, keyVersion int) []byte {
	return []byte(fmt.Sprintf("%s|%s|%d|%s|%d", tenantID, secretID, versionNo, name, keyVersion))
}

// innerAAD binds the wrapped DEK to row identity. Layout per design §3.
// Excludes `name` because the DEK wrap doesn't need to be bound to
// the secret's logical name — only to its row identity.
func innerAAD(tenantID, secretID string, versionNo, keyVersion int) []byte {
	return []byte(fmt.Sprintf("%s|%s|%d|%d", tenantID, secretID, versionNo, keyVersion))
}

// validateName checks the on-disk shape constraint (see nameRE).
// The unique index COALESCE-binds NULL stack so name is the primary
// discriminator within a tenant.
func validateName(name string) error {
	if !nameRE.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

// nilIfEmptyStack normalizes a *string stack pointer for SQL binding.
// SQL NULL is the on-disk representation of "tenant-wide"; "" in Go
// is treated identically by the COALESCE'd unique index so it's
// safe to pass either shape. We bind nil explicitly to keep the
// distinction visible in DB inspection (`SELECT … WHERE stack IS NULL`).
func nilIfEmptyStack(stack *string) any {
	if stack == nil || *stack == "" {
		return nil
	}
	return *stack
}

// copyStringPtr returns a pointer to a copy of the string pointed to
// by p, or nil. Used to avoid aliasing caller-owned pointers in
// returned metadata.
func copyStringPtr(p *string) *string {
	if p == nil {
		return nil
	}
	s := *p
	return &s
}

// isUniqueViolation matches SQLite's UNIQUE-constraint error text.
// Mirrors the dialect-seam pattern used by chassis/auth/registry —
// kept inline here because runtime tables are SQLite-only (see
// the Store doc comment).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "constraint failed: UNIQUE")
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanMetadata works
// for both single-row lookups and list iteration.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanMetadata(r rowScanner) (*SecretMetadata, error) {
	var m SecretMetadata
	var stack sql.NullString
	var lastRotated, revokedAt sql.NullString
	var createdAtStr string
	if err := r.Scan(
		&m.SecretID, &m.TenantID, &stack, &m.Name, &m.Description,
		&createdAtStr, &m.CreatedBy, &lastRotated, &revokedAt, &m.KeyVersion,
		&m.VersionNo,
	); err != nil {
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
		m.CreatedAt = t
	}
	if stack.Valid {
		s := stack.String
		m.Stack = &s
	}
	if lastRotated.Valid {
		if t, err := time.Parse(time.RFC3339, lastRotated.String); err == nil {
			m.LastRotatedAt = &t
		}
	}
	if revokedAt.Valid {
		if t, err := time.Parse(time.RFC3339, revokedAt.String); err == nil {
			m.RevokedAt = &t
		}
	}
	return &m, nil
}
