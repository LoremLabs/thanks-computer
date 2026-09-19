package authn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
)

// The Resolver is the one login path for every head that signs someone in
// with a password: IMAP, CalDAV, CardDAV and WebDAV. Each head still owns
// its protocol — TLS, parsing the credentials, finding the account the
// username names, the account's own status, admission, the tenant match —
// and hands the resolver the part they share:
//
//	username ──binding──▶ principal ──short id──▶ credential ──argon2──▶ verified
//	                                                   │
//	                          scope must cover the door the head opens
//
// One resolver serves all four heads, so they share one login cache and
// one set of throttles. A guess made over CalDAV counts against the same
// budget as one made over IMAP; four heads do not give four times the
// guesses.

// Outcome is how a login ended, for the head's reply and its log line.
type Outcome string

const (
	OutcomeOK Outcome = "ok"
	// OutcomeFailed: an unknown username, a wrong password, a password the
	// chassis never issued (it has no credential id), or a revoked one. The
	// client learns only that it failed.
	OutcomeFailed Outcome = "failed"
	// OutcomeThrottled: over --login-rate for this client IP or principal.
	OutcomeThrottled Outcome = "throttled"
	// OutcomeScope: the right password for the wrong door — a credential
	// whose scopes do not cover what the head opens. Answered like a wrong
	// password; logged apart so an operator can see it.
	OutcomeScope Outcome = "scope"
	// OutcomeDisabled: the principal is a user who has been disabled.
	OutcomeDisabled Outcome = "disabled"
	// OutcomeError: the store failed. Answer "try again later", not "wrong
	// password" — a client that believes its password is wrong asks for it.
	OutcomeError Outcome = "error"
)

// Attempt is one login a head asks about.
type Attempt struct {
	// Tenant is the tenant SLUG the account belongs to — what the protocol
	// stores key on. The resolver turns it into the tenant id.
	Tenant string
	// Username is the account's login name, which an email binding resolves
	// to the principal.
	Username string
	Password string
	IP       string
	// Want is the door: imap:<username>:login, drive:<collection-id>:login…
	Want Scope
}

// Result is the answer to an Attempt.
type Result struct {
	Outcome Outcome
	// Who is set when Outcome is OutcomeOK.
	Who Authenticated
	// Cached: the password was verified recently on this node, so no argon2
	// ran. A head uses it to log one line per check, not one per request.
	Cached bool
	// Err is set when Outcome is OutcomeError.
	Err error
}

// ResolverConfig configures a Resolver.
type ResolverConfig struct {
	// Rate is the most password checks a minute, per client IP and per
	// principal, across every head (--login-rate). A cache hit is not a
	// check. 0 disables throttling.
	Rate int
	// TenantID resolves a tenant slug to the id the identity tables key on.
	TenantID func(ctx context.Context, slug string) (string, error)
}

const (
	// loginCacheTTL: how long a verified password is remembered on a node.
	// Revocation does not wait for it — every login reads the credential
	// row — so this bounds only how often argon2 runs.
	loginCacheTTL = 5 * time.Minute
	// loginCacheMax holds the four heads' clients together.
	loginCacheMax = 40000
)

// Resolver verifies logins against the identity store.
type Resolver struct {
	store     *Store
	tenantID  func(ctx context.Context, slug string) (string, error)
	cache     *apppass.LoginCache
	byIP      *throttle.Throttle
	bySubject *throttle.Throttle
}

// NewResolver builds the resolver the heads share.
func NewResolver(store *Store, cfg ResolverConfig) *Resolver {
	return &Resolver{
		store:     store,
		tenantID:  cfg.TenantID,
		cache:     apppass.NewLoginCache(loginCacheTTL, loginCacheMax),
		byIP:      throttle.New(cfg.Rate, time.Minute),
		bySubject: throttle.New(cfg.Rate, time.Minute),
	}
}

// allow spends one password check from the client IP's budget and the
// subject's (a principal, or an unbound username).
func (r *Resolver) allow(ip, subject string) bool {
	if ip != "" {
		if ok, _ := r.byIP.Allow(ip); !ok {
			return false
		}
	}
	if ok, _ := r.bySubject.Allow(subject); !ok {
		return false
	}
	return true
}

// Miss answers a login for a username the head found no account for. It
// costs what a real check costs — the throttles, then one argon2 verify —
// so a reply's timing does not say whether an account exists.
func (r *Resolver) Miss(ip, username, password string) Outcome {
	if r == nil {
		apppass.VerifyDummy(password)
		return OutcomeFailed
	}
	if !r.allow(ip, "u:"+strings.ToLower(username)) {
		return OutcomeThrottled
	}
	apppass.VerifyDummy(password)
	return OutcomeFailed
}

// Login verifies one attempt.
func (r *Resolver) Login(ctx context.Context, a Attempt) Result {
	if r == nil || r.store == nil || r.tenantID == nil {
		return Result{Outcome: OutcomeError, Err: errors.New("authn: no identity store on this node")}
	}
	tenantID, err := r.tenantID(ctx, a.Tenant)
	if err != nil || tenantID == "" {
		return Result{Outcome: OutcomeError, Err: fmt.Errorf("authn: tenant %q: %v", a.Tenant, err)}
	}
	username, err := NormalizeEmail(a.Username)
	if err != nil {
		return Result{Outcome: r.Miss(a.IP, a.Username, a.Password)}
	}
	// A password the chassis did not issue has no credential id (shortID is
	// ""): the principal is still resolved, so it spends the same throttle
	// budget and fails like a wrong password, after the same work.
	shortID, _ := ShortIDOf(a.Password)

	row, err := r.store.loginRow(ctx, tenantID, username, shortID)
	switch {
	case errors.Is(err, ErrNotFound):
		// No binding: the account exists but was never given a principal
		// (an account made before the chassis bound usernames), or the
		// binding was revoked.
		return Result{Outcome: r.Miss(a.IP, tenantID+"/"+username, a.Password)}
	case err != nil:
		return Result{Outcome: OutcomeError, Err: err}
	}
	subject := "p:" + tenantID + "/" + row.principal.ID

	if row.credential.ID == "" {
		if !r.allow(a.IP, subject) {
			return Result{Outcome: OutcomeThrottled}
		}
		apppass.VerifyDummy(a.Password)
		return Result{Outcome: OutcomeFailed}
	}

	key := apppass.LoginKey(row.credential.ID, row.secretHash, a.Password)
	cached := r.cache.Hit(key)
	if !cached {
		if !r.allow(a.IP, subject) {
			return Result{Outcome: OutcomeThrottled}
		}
		match, err := apppass.VerifyPassword(row.secretHash, a.Password)
		if err != nil {
			return Result{Outcome: OutcomeError, Err: fmt.Errorf("authn: credential %s has an unreadable hash: %w", row.credential.ID, err)}
		}
		if !match {
			return Result{Outcome: OutcomeFailed}
		}
		r.cache.Put(key)
		// Once per check, not per request: a DAV client sends its password
		// with every request, and a cache hit is that same client.
		r.store.touchCredential(ctx, row.credential.ID)
	}
	if !row.credential.Scopes.Cover(a.Want) {
		return Result{Outcome: OutcomeScope, Cached: cached}
	}
	if row.userStatus != "" && row.userStatus != StatusActive {
		return Result{Outcome: OutcomeDisabled, Cached: cached}
	}
	return Result{
		Outcome: OutcomeOK,
		Who:     Authenticated{Principal: row.principal, Credential: row.credential.ID},
		Cached:  cached,
	}
}

// loginRow is what one login reads.
type loginRow struct {
	principal  Principal
	credential Credential // zero ID: no live credential with that short id
	secretHash string
	userStatus string // "" unless the principal is a user
}

// loginRow reads, in one query, the principal a username is bound to, that
// principal's live credential with the given short id (if any), and — for a
// user principal — the user's status. ErrNotFound when no live binding.
func (s *Store) loginRow(ctx context.Context, tenantID, username, shortID string) (loginRow, error) {
	var (
		pid                                     string
		cID, cShort, cKind, cScopes, cLabel     sql.NullString
		cBy, cCreated, cUsed, cHash, userStatus sql.NullString
	)
	err := s.qr(ctx, s.DB, `
		SELECT b.principal_id,
		       c.id, c.short_id, c.kind, c.scopes, c.label, c.created_by, c.created_at, c.last_used_at, c.secret_hash,
		       u.status
		  FROM principal_bindings b
		  LEFT JOIN credentials c
		    ON c.tenant_id = b.tenant_id AND c.principal_id = b.principal_id
		   AND c.short_id = ? AND c.revoked_at IS NULL
		  LEFT JOIN users u
		    ON b.principal_id LIKE 'user:%' AND u.id = substr(b.principal_id, 6) AND u.tenant_id = b.tenant_id
		 WHERE b.tenant_id = ? AND b.kind = 'email' AND COALESCE(b.issuer, '') = ''
		   AND b.subject = ? AND b.revoked_at IS NULL`,
		shortID, tenantID, username).
		Scan(&pid, &cID, &cShort, &cKind, &cScopes, &cLabel, &cBy, &cCreated, &cUsed, &cHash, &userStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return loginRow{}, ErrNotFound
	}
	if err != nil {
		return loginRow{}, err
	}
	kind, _, _ := strings.Cut(pid, ":")
	row := loginRow{principal: Principal{ID: pid, Kind: PrincipalKind(kind)}, userStatus: userStatus.String}
	if row.principal.Kind == KindUser && !userStatus.Valid {
		// A user principal whose users row is gone: nobody to sign in.
		row.userStatus = StatusDisabled
	}
	if !cID.Valid {
		return row, nil
	}
	scopes, err := decodeScopes(cScopes.String)
	if err != nil {
		return loginRow{}, fmt.Errorf("authn: credential %s: %w", cID.String, err)
	}
	row.credential = Credential{
		ID: cID.String, TenantID: tenantID, Principal: row.principal, ShortID: cShort.String,
		Kind: CredentialKind(cKind.String), Scopes: scopes, Label: cLabel.String, CreatedBy: cBy.String,
		CreatedAt: parseStamp(cCreated.String), LastUsedAt: parseStampPtr(cUsed),
	}
	row.secretHash = cHash.String
	return row, nil
}

// touchCredential records a verified use. Best effort: a login that
// succeeded does not fail because a timestamp could not be written.
func (s *Store) touchCredential(ctx context.Context, id string) {
	_, _ = s.ex(ctx, s.DB, `UPDATE credentials SET last_used_at = ? WHERE id = ?`, s.stamp(), id)
}
