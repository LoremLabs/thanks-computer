package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
)

// AuthMode controls which auth flavours the middleware accepts.
//
//	Basic:   only HTTP Basic; signed requests are rejected with 401
//	Signed:  only RFC 9421 signed; Basic is rejected with 401
//	Both:    accept whichever the request presents (preferring signed)
//
// In Basic mode with empty admin user/pass, the chassis falls through
// to "open dev" (current pre-auth behavior).
//
// Browser session cookies (`txco_session=<id>`) are accepted on top of
// any mode whenever Config.Sessions is non-nil — they're additive, not
// a mode of their own. The cookie path resolves the session through
// Sessions.GetSession and stamps a browser-Source auth.Context. CSRF
// is enforced via SameSite=Strict at the cookie level plus an Origin
// allowlist check on mutation methods (handled here).
type AuthMode string

const (
	ModeBasic  AuthMode = "basic"
	ModeSigned AuthMode = "signed"
	ModeBoth   AuthMode = "both"
)

// SessionStore is the minimal slice of registry behaviour the
// middleware needs to authenticate a browser session cookie. Keeping
// this as an interface (rather than importing the full registry type)
// avoids a cycle and lets tests swap in a fake.
type SessionStore interface {
	GetSession(ctx context.Context, sessionID string) (*registry.Session, error)
	TouchSession(ctx context.Context, sessionID string, now time.Time) error
}

// sessionCookieName is the literal `Set-Cookie: <name>=...` token the
// chassis emits and reads. Exported as a package constant so the
// admin server (which sets the cookie) and the middleware (which
// reads it) agree.
const SessionCookieName = "txco_session"

// Config bundles everything the middleware needs to authenticate a
// request. Built once at admin server startup and reused per request.
type Config struct {
	Mode AuthMode

	// Basic-auth credentials. When both are empty AND Mode != Signed,
	// the middleware enters open-dev mode for backward compat.
	BasicUser string
	BasicPass string

	// Signed-request inputs.
	Registry *registry.Registry
	Verifier signature.Verifier
	Nonces   *NonceStore
	Skew     time.Duration // clock-skew window for `created`; defaults to 5min

	// Browser-session inputs. When Sessions is non-nil, the middleware
	// also accepts `Cookie: txco_session=<id>`. AllowedOrigins lists
	// the Origin header values acceptable on mutation requests (POST/
	// PUT/PATCH/DELETE) carrying a session cookie — used as a CSRF
	// belt-and-braces on top of SameSite=Strict. An empty allowlist
	// disables the Origin check (only safe in tests).
	Sessions       SessionStore
	AllowedOrigins []string

	// Logger is optional; when nil the middleware stays quiet.
	Logger func(msg string, fields map[string]any)
}

// Middleware returns an http.Handler middleware that authenticates
// requests per Config.Mode and attaches an auth.Context to the request
// before calling next.
//
// Per the design doc, /healthz and /auth/dev/enroll bypass the auth
// middleware entirely — they're wired in front of this in server.go.
// Everything else goes through here.
func Middleware(cfg Config, next http.Handler) http.Handler {
	if cfg.Skew == 0 {
		cfg.Skew = 5 * time.Minute
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, err := cfg.authenticate(r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		if ctx != nil {
			r = r.WithContext(WithContext(r.Context(), ctx))
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate returns the auth.Context for the request, or an
// *signature.AuthError describing what went wrong. A nil context with
// nil error means "open dev" passthrough.
func (cfg *Config) authenticate(r *http.Request) (*Context, error) {
	hasSig := r.Header.Get("Signature-Input") != ""
	hasBasic := strings.HasPrefix(r.Header.Get("Authorization"), "Basic ")
	cookie := cfg.sessionCookie(r)

	switch cfg.Mode {
	case ModeSigned:
		if hasSig {
			return cfg.verifySigned(r)
		}
		if cookie != "" {
			return cfg.verifyCookie(r, cookie)
		}
		return nil, &signature.AuthError{Code: signature.ErrMissingSignatureHeaders}
	case ModeBasic:
		if hasBasic {
			return cfg.verifyBasic(r)
		}
		if cookie != "" {
			// Try the cookie, but DON'T hard-fail on a stale/invalid one:
			// fall through so an open-dev chassis (empty basic creds)
			// isn't locked out by a leftover cookie from a previous run.
			if ctx, err := cfg.verifyCookie(r, cookie); err == nil {
				return ctx, nil
			}
		}
		if cfg.BasicUser == "" && cfg.BasicPass == "" {
			return openDevContext(), nil
		}
		return nil, &signature.AuthError{Code: signature.ErrMissingSignatureHeaders, Cause: errors.New("Basic credentials required")}
	case ModeBoth:
		if hasSig {
			return cfg.verifySigned(r)
		}
		if cookie != "" {
			// Same passthrough as ModeBasic — a stale cookie shouldn't
			// 401 a chassis whose other auth sources accept the request.
			if ctx, err := cfg.verifyCookie(r, cookie); err == nil {
				return ctx, nil
			}
		}
		if hasBasic {
			return cfg.verifyBasic(r)
		}
		if cfg.BasicUser == "" && cfg.BasicPass == "" {
			return openDevContext(), nil
		}
		return nil, &signature.AuthError{Code: signature.ErrMissingSignatureHeaders}
	default:
		// Unknown mode: be safe, deny.
		return nil, &signature.AuthError{Code: signature.ErrMissingSignatureHeaders, Cause: errors.New("unknown auth mode")}
	}
}

// sessionCookie reads the session cookie value if one is present. Returns
// "" when no Sessions store is configured (browser auth disabled) or no
// cookie is on the request.
func (cfg *Config) sessionCookie(r *http.Request) string {
	if cfg.Sessions == nil {
		return ""
	}
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// verifyCookie resolves the session, runs the CSRF Origin check on
// mutation methods, and builds the auth.Context. Reused for any mode
// that admits cookies — a session is just a packaged signed identity,
// so it's accepted alongside whichever primary credential the mode
// recognises.
func (cfg *Config) verifyCookie(r *http.Request, sessionID string) (*Context, error) {
	sess, err := cfg.Sessions.GetSession(r.Context(), sessionID)
	if err != nil {
		// Treat any lookup failure (not found, DB error) as 401 — no
		// need to leak which case it was.
		return nil, &signature.AuthError{Code: signature.ErrInvalidSignature, Cause: errors.New("session invalid")}
	}
	if !sess.IsValid(time.Now().UTC()) {
		return nil, &signature.AuthError{Code: signature.ErrInvalidSignature, Cause: errors.New("session revoked or expired")}
	}
	if isMutationMethod(r.Method) && !cfg.originAllowed(r) {
		// CSRF defence: SameSite=Strict at the cookie layer already
		// blocks cross-site form posts in modern browsers, but
		// belt-and-suspenders the Origin header on every mutation.
		return nil, &signature.AuthError{Code: signature.ErrInvalidSignature, Cause: errors.New("csrf_origin_mismatch")}
	}
	// Touch last_seen_at on a goroutine so the request path isn't
	// blocked by a tiny write.
	if cfg.Sessions != nil {
		sid := sessionID
		go func() {
			_ = cfg.Sessions.TouchSession(context.Background(), sid, time.Now().UTC())
		}()
	}
	return &Context{
		Source:       "browser",
		ActorID:      sess.ActorID,
		TenantID:     sess.TenantID,
		Capabilities: append([]string(nil), sess.Capabilities...),
		SuperAdmin:   sess.SuperAdmin,
	}, nil
}

func isMutationMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// originAllowed checks the request's Origin header against the
// configured allowlist. Same-origin requests (no Origin header — e.g.
// fetch() from the same origin without `mode: cors`) are allowed:
// the cookie itself is the bearer of authority and SameSite=Strict
// is what blocks the cross-site case. When the Origin header IS
// present, it must match one of the allowlisted entries by
// scheme+host+port.
//
// An empty allowlist short-circuits to "allow" — used by tests and
// by chassis configurations that haven't been hardened yet.
func (cfg *Config) originAllowed(r *http.Request) bool {
	if len(cfg.AllowedOrigins) == 0 {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	got, err := url.Parse(origin)
	if err != nil {
		return false
	}
	for _, allowed := range cfg.AllowedOrigins {
		want, err := url.Parse(allowed)
		if err != nil {
			continue
		}
		if want.Scheme == got.Scheme && want.Host == got.Host {
			return true
		}
	}
	return false
}

// verifySigned runs the RFC 9421 verification pipeline + capability load.
func (cfg *Config) verifySigned(r *http.Request) (*Context, error) {
	if cfg.Registry == nil || cfg.Verifier == nil || cfg.Nonces == nil {
		return nil, &signature.AuthError{Code: signature.ErrInvalidSignature, Cause: errors.New("auth not configured")}
	}

	resolve := func(keyID string) (ed25519.PublicKey, error) {
		key, err := cfg.Registry.LookupKey(r.Context(), keyID)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				return nil, &signature.AuthError{Code: signature.ErrUnknownKey, Cause: err}
			}
			return nil, &signature.AuthError{Code: signature.ErrInvalidSignature, Cause: err}
		}
		if key.IsRevoked() {
			return nil, &signature.AuthError{Code: signature.ErrRevokedKey}
		}
		return key.PublicKey, nil
	}

	opts := signature.VerifyOptions{
		NotOlderThan: cfg.Skew,
		NonceCheck: func(keyID, nonce string) error {
			return cfg.Nonces.Use(r.Context(), keyID, nonce)
		},
	}

	verified, err := cfg.Verifier.Verify(r, resolve, opts)
	if err != nil {
		return nil, err
	}

	// Now load the actor + capabilities and assemble the context.
	key, err := cfg.Registry.LookupKey(r.Context(), verified.KeyID)
	if err != nil {
		return nil, &signature.AuthError{Code: signature.ErrUnknownKey, Cause: err}
	}
	actor, err := cfg.Registry.LookupActor(r.Context(), key.ActorID)
	if err != nil {
		return nil, &signature.AuthError{Code: signature.ErrActorRevoked, Cause: err}
	}
	if actor.RevokedAt != nil {
		return nil, &signature.AuthError{Code: signature.ErrActorRevoked}
	}
	// Capabilities start empty on every signed request. Tenant-scoped
	// routes have their resolveTenantMiddleware populate Capabilities
	// from the caller's membership in that tenant; chassis-wide routes
	// either don't check capabilities (whoami) or fall back to the
	// super_admin flag (revoke-key, tenant create). The old chassis-
	// wide actor_capabilities table was retired in phase 8b.
	return &Context{
		Source:     "signed",
		ActorID:    actor.ActorID,
		KeyID:      verified.KeyID,
		Tenant:     actor.Tenant,
		SuperAdmin: actor.SuperAdmin,
	}, nil
}

// verifyBasic checks the legacy basic-auth header against the
// configured admin user/pass. On success, synthesizes an in-memory
// auth.Context with admin:all — basic-auth callers are NEVER
// persisted to the actor tables.
func (cfg *Config) verifyBasic(r *http.Request) (*Context, error) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return nil, &signature.AuthError{Code: signature.ErrMissingSignatureHeaders, Cause: errors.New("malformed Authorization header")}
	}
	if subtle.ConstantTimeCompare([]byte(user), []byte(cfg.BasicUser)) != 1 ||
		subtle.ConstantTimeCompare([]byte(pass), []byte(cfg.BasicPass)) != 1 {
		return nil, &signature.AuthError{Code: signature.ErrInvalidSignature, Cause: errors.New("bad basic auth")}
	}
	return &Context{
		Source:       "basic",
		Capabilities: []string{"admin:all"},
	}, nil
}

func openDevContext() *Context {
	return &Context{
		Source:       "open",
		Capabilities: []string{"admin:all"},
	}
}

func writeAuthError(w http.ResponseWriter, err error) {
	code := signature.ErrInvalidSignature
	msg := "Request could not be authenticated."
	status := http.StatusUnauthorized
	var ae *signature.AuthError
	if errors.As(err, &ae) {
		code = ae.Code
		msg = humanMessage(ae.Code)
		if code == signature.ErrCapabilityDenied {
			status = http.StatusForbidden
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": msg,
	})
}

func humanMessage(code string) string {
	switch code {
	case signature.ErrMissingSignatureHeaders:
		return "Request is missing required signature headers."
	case signature.ErrUnknownKey:
		return "The signing key is not known to this chassis."
	case signature.ErrRevokedKey:
		return "The signing key has been revoked."
	case signature.ErrInvalidContentDigest:
		return "Content-Digest does not match the request body."
	case signature.ErrInvalidSignature:
		return "Request signature could not be verified."
	case signature.ErrTimestampOutOfRange:
		return "Signature timestamp is outside the allowed clock skew."
	case signature.ErrNonceReplay:
		return "Signature nonce has already been used."
	case signature.ErrActorRevoked:
		return "The actor associated with this key has been revoked."
	case signature.ErrCapabilityDenied:
		return "Caller lacks the required capability for this endpoint."
	default:
		return "Request could not be authenticated."
	}
}

// WriteForbidden is a small helper for handler-side capability checks
// so they can produce the same error shape as the middleware.
func WriteForbidden(w http.ResponseWriter, code string) {
	if code == "" {
		code = signature.ErrCapabilityDenied
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":   code,
		"message": humanMessage(code),
	})
}

// WriteForbiddenDetail is WriteForbidden plus a structured `detail` map for
// diagnosability — e.g. how the caller resolved (source/actor/caps) and what
// the endpoint required. The detail is the caller's OWN identity plus the
// required capability, returned to the already-authenticated caller, so it
// discloses nothing cross-tenant. Empty detail behaves like WriteForbidden.
func WriteForbiddenDetail(w http.ResponseWriter, code string, detail map[string]any) {
	if code == "" {
		code = signature.ErrCapabilityDenied
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	body := map[string]any{
		"error":   code,
		"message": humanMessage(code),
	}
	if len(detail) > 0 {
		body["detail"] = detail
	}
	_ = json.NewEncoder(w).Encode(body)
}
