package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// Browser-friendly auth: bootstrap tokens + session cookies.
//
// The CLI calls CreateBootstrap (signed) to mint a single-use token,
// hands the plaintext to the browser via a deep-link URL; the browser
// calls ConsumeBootstrap (unsigned, throttled at the HTTP layer) to
// trade it for a session, and the auth middleware then accepts the
// session cookie alongside signed/basic.
//
// Token shape: `btk_<base64url(32 random bytes)>` — 256 bits of
// entropy, stored as sha256 hex so the plaintext never persists.
// Session id shape: `bsn_<hxid time-sortable>`.

const (
	// bootstrapTokenPrefix is prepended to the base64url-encoded random
	// bytes so plain-text tokens are visually identifiable in logs.
	bootstrapTokenPrefix = "btk_"
	bootstrapRandomBytes = 32

	// sessionIDPrefix is on every session_id; useful both for grepping
	// audit logs and for parsing cookies defensively.
	sessionIDPrefix = "bsn_"
)

// Bootstrap is one row of browser_bootstrap.
type Bootstrap struct {
	TokenHash    string
	ActorID      string
	TenantID     string
	Capabilities []string
	Label        string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	ConsumedAt   *time.Time
	ConsumedIP   string
}

// Session is one row of browser_sessions.
type Session struct {
	SessionID    string
	ActorID      string
	TenantID     string
	Capabilities []string
	UA           string
	IP           string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	RevokedBy    string
	LastSeenAt   time.Time
}

// IsValid reports whether the session is currently usable: not revoked
// and not past its absolute expiry. The middleware calls this with
// time.Now() at the start of every request that carries a session
// cookie.
func (s *Session) IsValid(now time.Time) bool {
	if s == nil || s.RevokedAt != nil {
		return false
	}
	if !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt) {
		return false
	}
	return true
}

// ErrBootstrapInvalid covers all the "this token can't be redeemed"
// cases — never minted, expired, already consumed. We collapse them
// into one error to avoid leaking which subtype the caller hit, which
// would let an attacker probe token existence.
var ErrBootstrapInvalid = errors.New("bootstrap token invalid or expired")

// hashBootstrapToken returns the at-rest representation of a plaintext
// bootstrap token. Same function is used to compute the lookup key
// on consume.
func hashBootstrapToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// newBootstrapToken returns a fresh (plaintext, hash) pair from
// crypto/rand. The plaintext is base64url with `btk_` prefix.
func newBootstrapToken() (plaintext, hash string, err error) {
	raw := make([]byte, bootstrapRandomBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("read random bytes: %w", err)
	}
	plaintext = bootstrapTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	hash = hashBootstrapToken(plaintext)
	return plaintext, hash, nil
}

// CreateBootstrap mints a new exchange token for the given actor +
// tenant + capability snapshot. Returns the plaintext token (which
// the caller surfaces to the user exactly once) and its expiry.
//
// Multiple outstanding bootstraps for the same actor are allowed —
// the short TTL bounds the blast radius and forbidding it would break
// retry flows.
func (r *Registry) CreateBootstrap(ctx context.Context, actorID, tenantID string, caps []string, label string, ttl time.Duration) (plaintext string, expiresAt time.Time, err error) {
	plaintext, hash, err := newBootstrapToken()
	if err != nil {
		return "", time.Time{}, err
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("marshal capabilities: %w", err)
	}
	now := time.Now().UTC()
	expiresAt = now.Add(ttl)
	if _, err := r.ex(ctx, r.DB,
		`INSERT INTO browser_bootstrap
		     (token_hash, actor_id, tenant_id, capabilities_json, label, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hash, actorID, tenantID, string(capsJSON), nullable(label),
		formatTime(now), formatTime(expiresAt)); err != nil {
		return "", time.Time{}, fmt.Errorf("insert bootstrap: %w", err)
	}
	return plaintext, expiresAt, nil
}

// ConsumeBootstrap is the one-shot redeem: matches the plaintext
// against the stored hash, marks the row consumed inside a single
// transaction, and returns the row to the caller so they can mint a
// session. Returns ErrBootstrapInvalid for any miss — not-found,
// expired, already-consumed — so the wire response can't be used to
// probe which tokens exist.
//
// The conditional UPDATE mirrors the SQLite pattern documented in
// `feedback_sqlite_transactions.md`: a SELECT alone races with
// another consumer; a `UPDATE … WHERE consumed_at IS NULL` + checking
// RowsAffected == 1 is atomic on the writer thread.
func (r *Registry) ConsumeBootstrap(ctx context.Context, plaintext, consumerIP string) (*Bootstrap, error) {
	hash := hashBootstrapToken(plaintext)
	now := time.Now().UTC()

	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Conditional update: only consume if still unconsumed and not yet
	// expired. Either condition failing returns 0 rows affected.
	res, err := r.ex(ctx, tx,
		`UPDATE browser_bootstrap
		    SET consumed_at = ?, consumed_ip = ?
		  WHERE token_hash = ?
		    AND consumed_at IS NULL
		    AND expires_at > ?`,
		formatTime(now), nullable(consumerIP), hash, formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("update bootstrap: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("rows affected: %w", err)
	}
	if affected != 1 {
		return nil, ErrBootstrapInvalid
	}

	// Load the row we just consumed so the caller can build a session.
	b, err := scanBootstrap(r.qr(ctx, tx,
		`SELECT token_hash, actor_id, tenant_id, capabilities_json,
		        COALESCE(label, ''), created_at, expires_at,
		        consumed_at, COALESCE(consumed_ip, '')
		   FROM browser_bootstrap WHERE token_hash = ?`, hash))
	if err != nil {
		return nil, fmt.Errorf("load bootstrap: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return b, nil
}

// CreateSession turns a successfully-consumed Bootstrap into a
// session. Capabilities are snapshotted from the bootstrap row, not
// re-resolved from memberships, so a capability change after this
// point requires a session revoke + fresh login to take effect.
func (r *Registry) CreateSession(ctx context.Context, b *Bootstrap, ua, ip string, ttl time.Duration) (*Session, error) {
	sessionID := sessionIDPrefix + hxid.NewTimeSort().String()
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	capsJSON, err := json.Marshal(b.Capabilities)
	if err != nil {
		return nil, fmt.Errorf("marshal capabilities: %w", err)
	}
	if _, err := r.ex(ctx, r.DB,
		`INSERT INTO browser_sessions
		     (session_id, actor_id, tenant_id, capabilities_json,
		      ua, ip, created_at, expires_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, b.ActorID, b.TenantID, string(capsJSON),
		nullable(ua), nullable(ip),
		formatTime(now), formatTime(expiresAt), formatTime(now)); err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return &Session{
		SessionID:    sessionID,
		ActorID:      b.ActorID,
		TenantID:     b.TenantID,
		Capabilities: append([]string(nil), b.Capabilities...),
		UA:           ua,
		IP:           ip,
		CreatedAt:    now,
		ExpiresAt:    expiresAt,
		LastSeenAt:   now,
	}, nil
}

// GetSession reads a session by id. Returns ErrNotFound when the
// session doesn't exist; returns a Session whose IsValid() is false
// when the row exists but is revoked or expired (caller decides how
// to surface that — middleware treats it as 401).
func (r *Registry) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	if !strings.HasPrefix(sessionID, sessionIDPrefix) {
		return nil, ErrNotFound
	}
	row := r.qr(ctx, r.DB,
		`SELECT session_id, actor_id, tenant_id, capabilities_json,
		        COALESCE(ua, ''), COALESCE(ip, ''),
		        created_at, expires_at, revoked_at,
		        COALESCE(revoked_by, ''), last_seen_at
		   FROM browser_sessions WHERE session_id = ?`, sessionID)
	return scanSession(row)
}

// RevokeSession marks a session revoked. Idempotent — calling it on
// an already-revoked or non-existent session is a no-op (returns
// nil), which simplifies UI logout flows that may double-fire.
func (r *Registry) RevokeSession(ctx context.Context, sessionID, by string) error {
	now := time.Now().UTC()
	_, err := r.ex(ctx, r.DB,
		`UPDATE browser_sessions
		    SET revoked_at = ?, revoked_by = ?
		  WHERE session_id = ? AND revoked_at IS NULL`,
		formatTime(now), nullable(by), sessionID)
	return err
}

// ListSessions returns every session for a tenant — both active and
// revoked/expired — newest first. The admin UI uses this to render a
// "manage sessions" view; the CLI uses it for `txco auth sessions list`.
func (r *Registry) ListSessions(ctx context.Context, tenantID string) ([]Session, error) {
	rows, err := r.qy(ctx, r.DB,
		`SELECT session_id, actor_id, tenant_id, capabilities_json,
		        COALESCE(ua, ''), COALESCE(ip, ''),
		        created_at, expires_at, revoked_at,
		        COALESCE(revoked_by, ''), last_seen_at
		   FROM browser_sessions
		  WHERE tenant_id = ?
		  ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// RevokeActorSessions revokes every session belonging to an actor.
// Called from the existing actor-revoke handler so revoking a user
// kicks them out of every browser they're signed into.
func (r *Registry) RevokeActorSessions(ctx context.Context, actorID, by string) error {
	now := time.Now().UTC()
	_, err := r.ex(ctx, r.DB,
		`UPDATE browser_sessions
		    SET revoked_at = ?, revoked_by = ?
		  WHERE actor_id = ? AND revoked_at IS NULL`,
		formatTime(now), nullable(by), actorID)
	return err
}

// TouchSession updates last_seen_at. Called from the middleware on
// every authenticated request. Cheap: indexed primary-key write of a
// single timestamp.
func (r *Registry) TouchSession(ctx context.Context, sessionID string, now time.Time) error {
	_, err := r.ex(ctx, r.DB,
		`UPDATE browser_sessions SET last_seen_at = ? WHERE session_id = ?`,
		formatTime(now), sessionID)
	return err
}

// --- scanners ---------------------------------------------------------

// rowScanner abstracts *sql.Row and *sql.Rows so scanBootstrap /
// scanSession can be reused.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanBootstrap(s rowScanner) (*Bootstrap, error) {
	var (
		b          Bootstrap
		createdAt  string
		expiresAt  string
		consumedAt sql.NullString
		consumedIP string
		capsJSON   string
	)
	if err := s.Scan(&b.TokenHash, &b.ActorID, &b.TenantID, &capsJSON,
		&b.Label, &createdAt, &expiresAt, &consumedAt, &consumedIP); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(capsJSON), &b.Capabilities); err != nil {
		return nil, fmt.Errorf("unmarshal capabilities: %w", err)
	}
	b.CreatedAt = parseTime(createdAt)
	b.ExpiresAt = parseTime(expiresAt)
	if consumedAt.Valid {
		t := parseTime(consumedAt.String)
		b.ConsumedAt = &t
	}
	b.ConsumedIP = consumedIP
	return &b, nil
}

func scanSession(s rowScanner) (*Session, error) {
	var (
		sess       Session
		capsJSON   string
		createdAt  string
		expiresAt  string
		revokedAt  sql.NullString
		lastSeenAt string
	)
	if err := s.Scan(&sess.SessionID, &sess.ActorID, &sess.TenantID, &capsJSON,
		&sess.UA, &sess.IP,
		&createdAt, &expiresAt, &revokedAt, &sess.RevokedBy, &lastSeenAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(capsJSON), &sess.Capabilities); err != nil {
		return nil, fmt.Errorf("unmarshal capabilities: %w", err)
	}
	sess.CreatedAt = parseTime(createdAt)
	sess.ExpiresAt = parseTime(expiresAt)
	sess.LastSeenAt = parseTime(lastSeenAt)
	if revokedAt.Valid {
		t := parseTime(revokedAt.String)
		sess.RevokedAt = &t
	}
	return &sess, nil
}

// --- time helper -----------------------------------------------------

const timeFormat = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
}
