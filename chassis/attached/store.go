package attached

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// Code lifetimes: an hour unless asked, a day at most.
const (
	DefaultCodeTTL = time.Hour
	MaxCodeTTL     = 24 * time.Hour
)

var (
	// ErrBadCode: no such code, or it is spent or expired. One answer for
	// all three: a caller learns nothing about which.
	ErrBadCode = errors.New("attached: the code is not valid")
	// ErrNameInUse: the provider holds a live workspace of this name.
	// Enrolling never destroys anything; the owner picks another name or
	// destroys that workspace first.
	ErrNameInUse = errors.New("attached: a workspace of that name already exists")
)

// Credential is one key enrolled for exactly one workspace.
type Credential struct {
	ID          string
	WorkspaceID string
	Tenant      string
	Stack       string
	Name        string
	PublicKey   ed25519.PublicKey
	Fingerprint string
	Label       string
	OS          string
	Root        string
	Machine     string
	CreatedAt   time.Time
	RevokedAt   *time.Time
}

// Revoked reports whether the credential may no longer hold its workspace.
func (c *Credential) Revoked() bool { return c == nil || c.RevokedAt != nil }

// Spec is the workspace the credential names.
func (c *Credential) Spec() workspace.Spec {
	return workspace.Spec{Tenant: c.Tenant, Stack: c.Stack, Name: c.Name}
}

// Fingerprint is a key's id: SHA256:… over the SSH wire form, as ssh-keygen
// prints it. It is the keyid of the signed enrollment.
func Fingerprint(pub ed25519.PublicKey) string {
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(sshPub)
}

// Store is the durable half of attach: codes, credentials, and the
// workspace row that says a name is attached. It lives in the runtime DB.
type Store struct {
	db      *sql.DB
	dialect registry.Dialect
	now     func() time.Time
}

// NewStore builds a Store over the opened runtime DB. A nil dialect is
// SQLite.
func NewStore(db *sql.DB, d registry.Dialect) *Store {
	if d == nil {
		d = registry.SQLite
	}
	return &Store{db: db, dialect: d, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Store) rb(q string) string { return s.dialect.Rebind(q) }

// EnsureSchema creates the attach tables if absent. Prod gets them from the
// runtime migration (0031 / the overlay's own); this is for tests and for a
// chassis that opens the DB without the migration runner.
func (s *Store) EnsureSchema(ctx context.Context) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS attach_codes (
			code_hash    TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			tenant       TEXT NOT NULL,
			stack        TEXT NOT NULL,
			name         TEXT NOT NULL,
			label        TEXT,
			created_at   TEXT NOT NULL,
			expires_at   TEXT NOT NULL,
			consumed_at  TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS attach_credentials (
			credential_id TEXT PRIMARY KEY,
			workspace_id  TEXT NOT NULL,
			tenant        TEXT NOT NULL,
			stack         TEXT NOT NULL,
			name          TEXT NOT NULL,
			public_key    TEXT NOT NULL,
			fingerprint   TEXT NOT NULL,
			label         TEXT,
			os            TEXT,
			root          TEXT,
			machine       TEXT,
			created_at    TEXT NOT NULL,
			revoked_at    TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS attach_credentials_live_idx
			ON attach_credentials (workspace_id) WHERE revoked_at IS NULL`,
	} {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("attached: ensure schema: %w", err)
		}
	}
	return nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// CreateCode makes a one-use code for the workspace spec names and returns
// it, once: only its hash is kept. It refuses a name the provider holds, so
// the owner hears of it now and not on their machine.
func (s *Store) CreateCode(ctx context.Context, spec workspace.Spec, label string, ttl time.Duration) (code string, expires time.Time, err error) {
	if spec.Tenant == "" || spec.Stack == "" {
		return "", time.Time{}, errors.New("attached: a code needs a tenant and a stack")
	}
	if err := workspace.ValidateName(spec.Name); err != nil {
		return "", time.Time{}, err
	}
	if ttl <= 0 {
		ttl = DefaultCodeTTL
	}
	if ttl > MaxCodeTTL {
		ttl = MaxCodeTTL
	}
	wid := workspace.ID(spec.Tenant, spec.Stack, spec.Name)
	if inUse, err := s.nameInUse(ctx, s.db, wid); err != nil {
		return "", time.Time{}, err
	} else if inUse {
		return "", time.Time{}, ErrNameInUse
	}
	code, err = auth.EightWordSecret()
	if err != nil {
		return "", time.Time{}, err
	}
	now := s.now()
	expires = now.Add(ttl)
	_, err = s.db.ExecContext(ctx, s.rb(`
		INSERT INTO attach_codes (code_hash, workspace_id, tenant, stack, name, label, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`),
		registry.HashToken(code), wid, spec.Tenant, spec.Stack, spec.Name, label, ts(now), ts(expires))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("attached: create code: %w", err)
	}
	return code, expires, nil
}

// CodeInfo is what a code says before it is spent.
type CodeInfo struct {
	Label     string
	ExpiresAt time.Time
}

// PeekCode reads a live code without spending it, so `txco attach` can say
// what it is about to attach to before the owner accepts.
func (s *Store) PeekCode(ctx context.Context, code string) (*CodeInfo, error) {
	var label sql.NullString
	var expires string
	err := s.db.QueryRowContext(ctx, s.rb(`
		SELECT label, expires_at FROM attach_codes
		 WHERE code_hash = ? AND consumed_at IS NULL AND expires_at > ?`),
		registry.HashToken(code), ts(s.now())).Scan(&label, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBadCode
	}
	if err != nil {
		return nil, fmt.Errorf("attached: peek code: %w", err)
	}
	info := &CodeInfo{Label: label.String}
	info.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
	return info, nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// nameInUse reports whether a live row of another provider holds the id.
func (s *Store) nameInUse(ctx context.Context, q querier, wid string) (bool, error) {
	var provider, status string
	err := q.QueryRowContext(ctx, s.rb(
		`SELECT provider, status FROM workspaces WHERE workspace_id = ?`), wid).Scan(&provider, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("attached: read workspace row: %w", err)
	}
	return status != workspace.StatusDestroyed && provider != workspace.ProviderAttached, nil
}

// Enrollment is what a machine says of itself when it enrolls. Only the key
// decides anything; the rest is shown back to the owner.
type Enrollment struct {
	PublicKey ed25519.PublicKey
	Label     string
	OS        string
	Root      string
	Machine   string
}

// Enroll spends code and makes its key the one credential for the code's
// workspace. In ONE transaction:
//
//   - the code is spent, by an UPDATE that must touch exactly one row;
//   - a name the provider holds is refused (and the code is not spent);
//   - any earlier credential for the workspace is revoked;
//   - the new credential is written;
//   - the workspace's row becomes the attached provider's.
//
// The caller must already have verified that the request was signed by
// e.PublicKey over these exact bytes. Enroll trusts its arguments.
func (s *Store) Enroll(ctx context.Context, code string, e Enrollment) (cred *Credential, replaced []string, err error) {
	if len(e.PublicKey) != ed25519.PublicKeySize {
		return nil, nil, errors.New("attached: enroll needs an Ed25519 public key")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("attached: enroll: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := s.now()
	hash := registry.HashToken(code)
	res, err := tx.ExecContext(ctx, s.rb(`
		UPDATE attach_codes SET consumed_at = ?
		 WHERE code_hash = ? AND consumed_at IS NULL AND expires_at > ?`),
		ts(now), hash, ts(now))
	if err != nil {
		return nil, nil, fmt.Errorf("attached: spend code: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, nil, ErrBadCode
	}
	c := &Credential{
		PublicKey:   e.PublicKey,
		Fingerprint: Fingerprint(e.PublicKey),
		OS:          clip(e.OS, 64),
		Root:        clip(e.Root, 1024),
		Machine:     clip(e.Machine, 128),
		CreatedAt:   now,
	}
	var codeLabel sql.NullString
	if err := tx.QueryRowContext(ctx, s.rb(
		`SELECT workspace_id, tenant, stack, name, label FROM attach_codes WHERE code_hash = ?`), hash).
		Scan(&c.WorkspaceID, &c.Tenant, &c.Stack, &c.Name, &codeLabel); err != nil {
		return nil, nil, fmt.Errorf("attached: read code: %w", err)
	}
	// The label is the owning stack's word for the name, never the machine's.
	c.Label = codeLabel.String

	if inUse, err := s.nameInUse(ctx, tx, c.WorkspaceID); err != nil {
		return nil, nil, err
	} else if inUse {
		return nil, nil, ErrNameInUse // rolled back: the code is still good
	}

	rows, err := tx.QueryContext(ctx, s.rb(
		`SELECT credential_id FROM attach_credentials WHERE workspace_id = ? AND revoked_at IS NULL`), c.WorkspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("attached: read credentials: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, nil, err
		}
		replaced = append(replaced, id)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, s.rb(
		`UPDATE attach_credentials SET revoked_at = ? WHERE workspace_id = ? AND revoked_at IS NULL`),
		ts(now), c.WorkspaceID); err != nil {
		return nil, nil, fmt.Errorf("attached: revoke earlier credential: %w", err)
	}

	if c.ID, err = newCredentialID(); err != nil {
		return nil, nil, err
	}
	if _, err := tx.ExecContext(ctx, s.rb(`
		INSERT INTO attach_credentials
			(credential_id, workspace_id, tenant, stack, name, public_key, fingerprint, label, os, root, machine, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		c.ID, c.WorkspaceID, c.Tenant, c.Stack, c.Name,
		base64.StdEncoding.EncodeToString(c.PublicKey), c.Fingerprint, c.Label, c.OS, c.Root, c.Machine, ts(now)); err != nil {
		return nil, nil, fmt.Errorf("attached: write credential: %w", err)
	}

	// The row is what says the name is attached. provider_ref names the
	// credential, so the Manager hands it to us without a second lookup.
	if _, err := tx.ExecContext(ctx, s.rb(`
		INSERT INTO workspaces
			(workspace_id, tenant, stack, name, provider, provider_ref, runtime, network,
			 status, run_id, checkpoint_ref, created_at, last_used_at, destroyed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL, 'public', ?, NULL, NULL, ?, ?, NULL)
		ON CONFLICT (tenant, stack, name) DO UPDATE SET
			provider       = excluded.provider,
			provider_ref   = excluded.provider_ref,
			runtime        = NULL,
			status         = excluded.status,
			run_id         = NULL,
			checkpoint_ref = NULL,
			last_used_at   = excluded.last_used_at,
			destroyed_at   = NULL`),
		c.WorkspaceID, c.Tenant, c.Stack, c.Name, workspace.ProviderAttached, c.ID,
		workspace.StatusCreated, ts(now), ts(now)); err != nil {
		return nil, nil, fmt.Errorf("attached: write workspace row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("attached: enroll: %w", err)
	}
	return c, replaced, nil
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max]
	}
	return s
}

func newCredentialID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "atc-" + hex.EncodeToString(b[:]), nil
}

const credCols = `credential_id, workspace_id, tenant, stack, name, public_key, fingerprint,
	label, os, root, machine, created_at, revoked_at`

func scanCredential(sc interface{ Scan(...any) error }) (*Credential, error) {
	var c Credential
	var pub, created string
	var label, os, root, machine, revoked sql.NullString
	if err := sc.Scan(&c.ID, &c.WorkspaceID, &c.Tenant, &c.Stack, &c.Name, &pub, &c.Fingerprint,
		&label, &os, &root, &machine, &created, &revoked); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(pub)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("attached: credential %s has a corrupt key", c.ID)
	}
	c.PublicKey = ed25519.PublicKey(raw)
	c.Label, c.OS, c.Root, c.Machine = label.String, os.String, root.String, machine.String
	c.CreatedAt, _ = time.Parse(time.RFC3339, created)
	if revoked.Valid && revoked.String != "" {
		if t, err := time.Parse(time.RFC3339, revoked.String); err == nil {
			c.RevokedAt = &t
		}
	}
	return &c, nil
}

// Credential returns the credential by id, revoked or not; (nil, nil) when
// there is none. This is the read the holder makes before every request:
// the DB, each time, never a copy kept in memory.
func (s *Store) Credential(ctx context.Context, id string) (*Credential, error) {
	c, err := scanCredential(s.db.QueryRowContext(ctx, s.rb(
		`SELECT `+credCols+` FROM attach_credentials WHERE credential_id = ?`), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("attached: read credential: %w", err)
	}
	return c, nil
}

// Live returns the one unrevoked credential for a workspace; (nil, nil)
// when there is none.
func (s *Store) Live(ctx context.Context, workspaceID string) (*Credential, error) {
	c, err := scanCredential(s.db.QueryRowContext(ctx, s.rb(
		`SELECT `+credCols+` FROM attach_credentials WHERE workspace_id = ? AND revoked_at IS NULL`), workspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("attached: read credential: %w", err)
	}
	return c, nil
}

// Revoke revokes the workspace's live credential and returns its id ("" when
// there was none). The row stays attached, so the name reads away, and is
// not handed back to the provider until the workspace is destroyed.
//
// From the moment this commits no request is forwarded to the machine: the
// holder reads the credential before each one.
func (s *Store) Revoke(ctx context.Context, workspaceID string) (string, error) {
	c, err := s.Live(ctx, workspaceID)
	if err != nil || c == nil {
		return "", err
	}
	res, err := s.db.ExecContext(ctx, s.rb(
		`UPDATE attach_credentials SET revoked_at = ? WHERE credential_id = ? AND revoked_at IS NULL`),
		ts(s.now()), c.ID)
	if err != nil {
		return "", fmt.Errorf("attached: revoke: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", nil // revoked by someone else in between
	}
	return c.ID, nil
}

// Enrolled reports whether the workspace's row says a machine holds the
// name, and the live credential if there is one (none after a revoke).
func (s *Store) Enrolled(ctx context.Context, spec workspace.Spec) (enrolled bool, cred *Credential, err error) {
	wid := workspace.ID(spec.Tenant, spec.Stack, spec.Name)
	var provider, status string
	err = s.db.QueryRowContext(ctx, s.rb(
		`SELECT provider, status FROM workspaces WHERE workspace_id = ?`), wid).Scan(&provider, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, fmt.Errorf("attached: read workspace row: %w", err)
	}
	if provider != workspace.ProviderAttached || status == workspace.StatusDestroyed {
		return false, nil, nil
	}
	cred, err = s.Live(ctx, wid)
	return true, cred, err
}
