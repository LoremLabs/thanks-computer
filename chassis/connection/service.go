package connection

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// Service is the chassis's side of a connection: it starts and finishes a
// sign-in, hands a request its token (renewing first when due), lists and
// revokes. The ops (chassis/server/connection.go) and the https op
// (chassis/processor/http.go) call it; nothing else sees material.

// Decls finds the declarations: a provider by name across the tenant's
// active stacks, and a stack's consumer declaration for a provider.
type Decls interface {
	// Provider resolves (tenant slug, name) → the declaring stack and its
	// active declaration, or ErrNotDeclared.
	Provider(ctx context.Context, tenant, name string) (string, *Provider, error)
	// Consumer resolves (tenant slug, stack, provider) → the stack's active
	// consumer declaration, or ErrNotDeclared.
	Consumer(ctx context.Context, tenant, stack, provider string) (*Consumer, error)
}

// Secrets materializes a tenant secret a declaration references, in the
// declaring stack's scope (the tenant secret store's own fallback rule).
type Secrets interface {
	Materialize(ctx context.Context, tenant, stack, name string) ([]byte, error)
}

// Config is what New needs beside the store and the seams.
type Config struct {
	// Node is the node's egress guard; Open reports that its policy is
	// open (development), which allows loopback origins.
	Node egress.Guard
	Open bool
	// SlugToID resolves a tenant slug (what a request pins) to the tenant
	// id the rows key on.
	SlugToID  func(ctx context.Context, slug string) (string, error)
	UserAgent string
	Logger    *zap.Logger
}

// Service — see the package comment.
type Service struct {
	store     *Store
	decls     Decls
	secrets   Secrets
	signer    *Signer
	clients   *clients
	slugToID  func(ctx context.Context, slug string) (string, error)
	userAgent string
	log       *zap.Logger
	now       func() time.Time
}

// New builds the service. signer is keyed from the master key under
// KeyLabel (secrets.DeriveKey).
func New(store *Store, decls Decls, sec Secrets, signer *Signer, cfg Config) (*Service, error) {
	if store == nil || decls == nil || sec == nil || signer == nil || cfg.SlugToID == nil {
		return nil, errors.New("connection: store, declarations, secrets, signer and SlugToID are all required")
	}
	if cfg.Node == nil {
		return nil, errors.New("connection: an egress guard is required")
	}
	cl, err := newClients(cfg.Node, cfg.Open)
	if err != nil {
		return nil, err
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "txco"
	}
	log := cfg.Logger
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{store: store, decls: decls, secrets: sec, signer: signer, clients: cl, slugToID: cfg.SlugToID, userAgent: ua, log: log, now: store.now}, nil
}

// SetClock replaces the service's and its store's clock (tests).
func (s *Service) SetClock(now func() time.Time) {
	s.now = now
	s.store.SetClock(now)
}

// Error is what a caller shapes for a rule: a code (without the
// `txco_connection_` prefix the op adds) and a message that never holds
// material.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func fail(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Error codes.
const (
	CodeInvalidArg  = "invalid_arg"
	CodeNotDeclared = "not_declared"
	CodeUnsupported = "unsupported"
	CodeNoClient    = "no_client" // the client id or secret is not set
	CodeStore       = "store"
	CodeBadState    = "bad_state"
	CodeExpired     = "expired"
	CodeGone        = "gone" // the flow was used or has expired
	CodeMismatch    = "mismatch"
	CodeRefused     = "refused" // the provider refused the code or the sign-in
	CodeUnavailable = "unavailable"
	CodeMissing     = "missing"
	CodeReconnect   = "reconnect"
	CodeBusy        = "busy"
	CodeDestination = "destination"
	CodeDenied      = "denied"
)

// CookieName is the per-flow browser cookie `start` sets and `finish`
// reads. Host-only (no Domain), so a callback on another host sees none.
const CookieName = "txco_connect"

// baseStack reduces a dispatching stack name to the one that owns rows:
// its first slash segment (authn.BaseStack, without the import).
func baseStack(stack string) string {
	stack = strings.TrimLeft(strings.TrimSpace(stack), "/")
	if i := strings.IndexByte(stack, '/'); i >= 0 {
		return stack[:i]
	}
	return stack
}

// creds materializes the provider's client id and secret in the declaring
// stack's scope.
func (s *Service) creds(ctx context.Context, tenant, declStack string, p *Provider) (clientCreds, *Error) {
	var c clientCreds
	if name, ok := SecretName(p.ClientID); ok {
		v, err := s.secrets.Materialize(ctx, tenant, declStack, name)
		if err != nil {
			return c, fail(CodeNoClient, "the provider's client_id (tenant secret %s) is not set: %v", name, err)
		}
		c.id = string(v)
	} else {
		c.id = p.ClientID
	}
	name, _ := SecretName(p.ClientSecret)
	v, err := s.secrets.Materialize(ctx, tenant, declStack, name)
	if err != nil {
		return c, fail(CodeNoClient, "the provider's client_secret (tenant secret %s) is not set: %v", name, err)
	}
	c.secret = string(v)
	return c, nil
}

func (s *Service) event(ctx context.Context, name string, fields map[string]any) {
	trace.FromContext(ctx).Event(trace.TimelineEvent{Ts: s.now(), Event: name, Fields: fields})
	zf := make([]zap.Field, 0, len(fields))
	for k, v := range fields {
		zf = append(zf, zap.Any(k, v))
	}
	s.log.Info(name, zf...)
}

// ── start ────────────────────────────────────────────────────────────────

// StartInput is what the managing stack asks for.
type StartInput struct {
	Tenant, TenantID string
	Stack            string // the managing stack, as dispatched (a slot reduces to its base)
	Principal        string
	Provider         string
	Kind             string // "" is oauth2
	Label            string // "" in this slice
	RedirectURI      string
	Host             string // the request's host: where the cookie goes
	Initiator        string // who the stack says is connecting, for the record
}

// StartResult is the sign-in URL and the cookie the response must set.
type StartResult struct {
	URL       string
	ExpiresAt time.Time
	Cookie    string // a Set-Cookie line
	Provider  *Provider
}

// Start records a flow and builds the provider's sign-in URL.
func (s *Service) Start(ctx context.Context, in StartInput) (StartResult, *Error) {
	if !ValidName(in.Provider) {
		return StartResult{}, fail(CodeInvalidArg, "`provider` %q is not a provider name", in.Provider)
	}
	switch in.Kind {
	case "", CredentialOAuth2:
		in.Kind = CredentialOAuth2
	case CredentialInstallation:
		return StartResult{}, fail(CodeUnsupported, "kind %s is not connected this way yet", in.Kind)
	default:
		return StartResult{}, fail(CodeInvalidArg, "`kind` %q: want %s", in.Kind, CredentialOAuth2)
	}
	if in.Label != "" {
		return StartResult{}, fail(CodeUnsupported, "one connection per provider for now: `label` must be empty")
	}
	stack := baseStack(in.Stack)
	if stack == "" {
		return StartResult{}, fail(CodeInvalidArg, "no stack in request scope")
	}
	declStack, p, err := s.decls.Provider(ctx, in.Tenant, in.Provider)
	if errors.Is(err, ErrNotDeclared) {
		return StartResult{}, fail(CodeNotDeclared, "no active stack of this tenant declares provider %q (PROVIDERS/%s.yaml)", in.Provider, in.Provider)
	}
	if err != nil {
		return StartResult{}, fail(CodeStore, "%v", err)
	}
	// A development node (its policy open) may send the browser back over
	// plain http, as it serves plain http; a fleet node never.
	redirect, perr := checkRedirectURI(in.RedirectURI, in.Host, in.Provider, s.clients.loopback != nil)
	if perr != nil {
		return StartResult{}, perr
	}
	creds, cerr := s.creds(ctx, in.Tenant, declStack, p)
	if cerr != nil {
		return StartResult{}, cerr
	}
	nonce, err := newNonce()
	if err != nil {
		return StartResult{}, fail(CodeStore, "%v", err)
	}
	cookieValue, browserHash, err := browserToken()
	if err != nil {
		return StartResult{}, fail(CodeStore, "%v", err)
	}
	now := s.now()
	expires := now.Add(FlowTTL)
	if err := s.store.PutFlow(ctx, Flow{
		NonceHash: nonceHash(nonce), TenantID: in.TenantID, PrincipalID: in.Principal, Provider: in.Provider, Label: in.Label,
		Kind: in.Kind, Stack: stack, RedirectURI: redirect.String(), BrowserHash: browserHash, Initiator: in.Initiator, ExpiresAt: expires,
	}); err != nil {
		return StartResult{}, fail(CodeStore, "%v", err)
	}
	state, err := s.signer.sign(stateClaims{Tenant: in.Tenant, Stack: stack, Provider: in.Provider, Nonce: nonce, Expires: expires.Unix()})
	if err != nil {
		return StartResult{}, fail(CodeStore, "%v", err)
	}
	auth, err := url.Parse(p.Destinations.Authorization)
	if err != nil {
		return StartResult{}, fail(CodeStore, "authorization destination: %v", err)
	}
	q := auth.Query()
	q.Set("response_type", "code")
	q.Set("client_id", creds.id)
	q.Set("redirect_uri", redirect.String())
	q.Set("state", state)
	if len(p.Scopes) > 0 {
		q.Set("scope", strings.Join(p.Scopes, " "))
	}
	if p.PKCE {
		q.Set("code_challenge", challenge(s.signer.verifier(nonce)))
		q.Set("code_challenge_method", "S256")
	}
	auth.RawQuery = q.Encode()
	s.event(ctx, "connection.started", map[string]any{"principal": in.Principal, "provider": in.Provider, "stack": stack})
	return StartResult{URL: auth.String(), ExpiresAt: expires, Cookie: setCookie(cookieValue, in.Host, int(FlowTTL.Seconds())), Provider: p}, nil
}

// checkRedirectURI: on the host serving the request, https (http to a
// loopback host, or anywhere on a development node), on a path that names
// the provider, no query or fragment.
func checkRedirectURI(raw, host, provider string, devNode bool) (*url.URL, *Error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, fail(CodeInvalidArg, "`redirect_uri` must be an absolute URL on this host")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (devNode || Loopback(strings.ToLower(u.Hostname())))) {
		return nil, fail(CodeInvalidArg, "`redirect_uri` must be https:// (http:// only to localhost, or on a development node)")
	}
	if !strings.EqualFold(u.Host, strings.TrimSpace(host)) {
		return nil, fail(CodeInvalidArg, "`redirect_uri` must be on the host serving this request (%s), not %s", host, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fail(CodeInvalidArg, "`redirect_uri` takes no query, fragment or user info")
	}
	if !strings.Contains(u.Path, "/"+provider+"/") && !strings.HasSuffix(u.Path, "/"+provider) {
		return nil, fail(CodeInvalidArg, "`redirect_uri` must name the provider on its path (…/%s/callback), so each provider has a callback of its own", provider)
	}
	return u, nil
}

func setCookie(value, host string, maxAge int) string {
	line := CookieName + "=" + value + "; Path=/; HttpOnly; SameSite=Lax; Max-Age=" + fmt.Sprint(maxAge)
	h := strings.ToLower(host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h, "]") {
		h = h[:i]
	}
	if !Loopback(h) {
		line += "; Secure"
	}
	return line
}

// ── finish ───────────────────────────────────────────────────────────────

// FinishInput is read from the callback request the managing stack is
// serving — never from a WITH clause.
type FinishInput struct {
	Tenant, TenantID string
	Stack            string
	Host, Path       string // the request's
	Code, State      string
	Iss              string
	ProviderError    string // the provider's `error` query parameter, if any
	Cookie           string // the flow cookie's value
}

// FinishResult is the stored connection's metadata and the cookie line
// that clears the flow cookie.
type FinishResult struct {
	Row         Row
	Provider    *Provider
	ClearCookie string
}

// Finish consumes the flow, exchanges the code and stores the tokens.
func (s *Service) Finish(ctx context.Context, in FinishInput) (FinishResult, *Error) {
	now := s.now()
	claims, err := s.signer.verify(in.State, now)
	switch {
	case errors.Is(err, ErrStateExpired):
		return FinishResult{}, fail(CodeExpired, "the sign-in took longer than %s: start again", FlowTTL)
	case err != nil:
		return FinishResult{}, fail(CodeBadState, "the state is not one this chassis signed")
	}
	stack := baseStack(in.Stack)
	if claims.Tenant != in.Tenant || claims.Stack != stack {
		return FinishResult{}, fail(CodeMismatch, "the sign-in was started by stack %q, not %q", claims.Stack, stack)
	}
	flow, err := s.store.ConsumeFlow(ctx, nonceHash(claims.Nonce))
	if errors.Is(err, ErrFlowGone) {
		return FinishResult{}, fail(CodeGone, "this sign-in was already used, or has expired: start again")
	}
	if err != nil {
		return FinishResult{}, fail(CodeStore, "%v", err)
	}
	// From here the flow is spent: any failure means starting again.
	clear := setCookie("", in.Host, 0)
	if in.Cookie == "" || subtle.ConstantTimeCompare([]byte(nonceHash(in.Cookie)), []byte(flow.BrowserHash)) != 1 {
		return FinishResult{ClearCookie: clear}, fail(CodeMismatch, "this browser did not start the sign-in")
	}
	if flow.Stack != stack || flow.Provider != claims.Provider || flow.TenantID != in.TenantID {
		return FinishResult{ClearCookie: clear}, fail(CodeMismatch, "the sign-in does not belong to this stack")
	}
	redirect, _ := url.Parse(flow.RedirectURI)
	if redirect == nil || !strings.EqualFold(redirect.Host, in.Host) || redirect.Path != in.Path {
		return FinishResult{ClearCookie: clear}, fail(CodeMismatch, "the callback arrived at %s%s, not at the sign-in's redirect_uri", in.Host, in.Path)
	}
	declStack, p, err := s.decls.Provider(ctx, in.Tenant, flow.Provider)
	if errors.Is(err, ErrNotDeclared) {
		return FinishResult{ClearCookie: clear}, fail(CodeNotDeclared, "no active stack declares provider %q any more", flow.Provider)
	}
	if err != nil {
		return FinishResult{ClearCookie: clear}, fail(CodeStore, "%v", err)
	}
	// The provider's `iss` (RFC 9207), when it sends one and the declaration
	// names an issuer: the same URL, whole. A declaration that names none
	// cannot check it; the state, the cookie and the PKCE verifier still
	// bind the callback to this sign-in.
	if in.Iss != "" && p.Issuer != "" && !SameIssuer(p.Issuer, in.Iss) {
		return FinishResult{ClearCookie: clear}, fail(CodeMismatch, "the callback's iss is not the provider's issuer")
	}
	if in.ProviderError != "" {
		return FinishResult{ClearCookie: clear}, fail(CodeRefused, "%s did not complete the sign-in: %s", providerTitle(p, flow.Provider), in.ProviderError)
	}
	if in.Code == "" {
		return FinishResult{ClearCookie: clear}, fail(CodeRefused, "the callback carried no code")
	}
	creds, cerr := s.creds(ctx, in.Tenant, declStack, p)
	if cerr != nil {
		return FinishResult{ClearCookie: clear}, cerr
	}
	a, xerr := s.exchange(ctx, p, creds, in.Code, s.signer.verifier(claims.Nonce), flow.RedirectURI)
	if xerr != nil {
		if a.Err != "" {
			return FinishResult{ClearCookie: clear}, fail(CodeRefused, "%s refused the code: %s", providerTitle(p, flow.Provider), a.Err)
		}
		return FinishResult{ClearCookie: clear}, fail(CodeUnavailable, "%s could not be reached to exchange the code: %v", providerTitle(p, flow.Provider), xerr)
	}
	subject, account, aerr := s.account(ctx, p, a.Access)
	if aerr != nil {
		return FinishResult{ClearCookie: clear}, fail(CodeUnavailable, "%s did not say who signed in: %v", providerTitle(p, flow.Provider), aerr)
	}
	expires, lifetime := expiry(now, a.ExpiresIn)
	refreshExpires, _ := expiry(now, a.RefreshExpiresIn)
	row, err := s.store.Put(ctx, NewRow{
		TenantID: flow.TenantID, PrincipalID: flow.PrincipalID, Provider: flow.Provider, Label: flow.Label, Kind: flow.Kind,
		Subject: subject, Account: account, Scopes: splitScopes(a.Scope, p.Scopes),
		ExpiresAt: expires, RefreshExpiresAt: refreshExpires, LifetimeS: lifetime, CreatedBy: stack,
	}, Material{Access: a.Access, Refresh: a.Refresh, TokenType: a.TokenType})
	if err != nil {
		return FinishResult{ClearCookie: clear}, fail(CodeStore, "%v", err)
	}
	s.event(ctx, "connection.connected", map[string]any{
		"principal": row.PrincipalID, "provider": row.Provider, "account": row.Account, "generation": row.Generation, "stack": stack,
	})
	return FinishResult{Row: row, Provider: p, ClearCookie: clear}, nil
}

func providerTitle(p *Provider, name string) string {
	if p != nil && p.Title != "" {
		return p.Title
	}
	return name
}

// expiry turns an expires_in into the instant and the lifetime; none when
// the provider gave none (the token does not expire).
func expiry(now time.Time, expiresIn int64) (*time.Time, int64) {
	if expiresIn <= 0 {
		return nil, 0
	}
	t := now.Add(time.Duration(expiresIn) * time.Second)
	return &t, expiresIn
}

// splitScopes reads the granted scopes (space- or comma-separated), falling
// back to what was asked when the provider says nothing.
func splitScopes(granted string, asked []string) []string {
	f := strings.FieldsFunc(granted, func(r rune) bool { return r == ' ' || r == ',' })
	if len(f) == 0 {
		return append([]string{}, asked...)
	}
	return f
}

// ── use ──────────────────────────────────────────────────────────────────

// UseInput is one request that names a connection: the op's own stack,
// the pinned principal and capability, and where the request goes.
type UseInput struct {
	Tenant     string
	Stack      string // the executing op's stack
	Provider   string
	Principal  string
	Capability string
	URL        *url.URL
}

// ReleaseInput is one sandbox entry that names this provider
// (`connection:<provider>`): whose connection, by which stack's sandbox.
// Every field is the run grant's own, never the asking program's.
type ReleaseInput struct {
	Tenant    string // slug
	Stack     string // the stack whose SANDBOXES/ declares the sandbox
	Provider  string
	Principal string
	Sandbox   string
}

// ReleaseCheck answers whether the principal holds a connection this
// stack's sandbox may release, without renewing or reading its material:
// what the grant gateway shows the tenant's rules before it decides. A
// nil error: release would find it.
func (s *Service) ReleaseCheck(ctx context.Context, in ReleaseInput) *Error {
	_, _, _, e := s.releaseRow(ctx, in)
	return e
}

// Release hands over the principal's current token at the provider for a
// sandbox, renewing it when due: the bare token, for the work that opened
// the sandbox to hold. The provider must name the stack's sandbox as a
// consumer (`<stack>: [sandbox:<name>]`). There is no destination check:
// the token is meant to leave for the machine the work runs on.
func (s *Service) Release(ctx context.Context, in ReleaseInput) (string, Row, *Error) {
	declStack, p, row, e := s.releaseRow(ctx, in)
	if e != nil {
		return "", Row{}, e
	}
	access, row, uerr := s.current(ctx, in.Tenant, declStack, p, row)
	if uerr != nil {
		return "", Row{}, uerr
	}
	return access, row, nil
}

func (s *Service) releaseRow(ctx context.Context, in ReleaseInput) (string, *Provider, Row, *Error) {
	if in.Principal == "" || in.Sandbox == "" {
		return "", nil, Row{}, fail(CodeDenied, "no principal and sandbox: a connection is released to work that opened a sandbox under a run grant")
	}
	if !ValidName(in.Provider) {
		return "", nil, Row{}, fail(CodeInvalidArg, "connection %q is not a provider name", in.Provider)
	}
	stack := baseStack(in.Stack)
	declStack, p, err := s.decls.Provider(ctx, in.Tenant, in.Provider)
	if errors.Is(err, ErrNotDeclared) {
		return "", nil, Row{}, fail(CodeDenied, "no active stack of this tenant declares provider %q", in.Provider)
	}
	if err != nil {
		return "", nil, Row{}, fail(CodeUnavailable, "%v", err)
	}
	if !p.Allows(stack, SandboxConsumerPrefix+in.Sandbox) {
		return "", nil, Row{}, fail(CodeDenied, "provider %q does not name stack %q as a consumer for %s%s", in.Provider, stack, SandboxConsumerPrefix, in.Sandbox)
	}
	tenantID, err := s.slugToID(ctx, in.Tenant)
	if err != nil {
		return "", nil, Row{}, fail(CodeUnavailable, "%v", err)
	}
	row, err := s.store.Get(ctx, tenantID, in.Principal, in.Provider, "")
	if errors.Is(err, ErrNotFound) {
		return "", nil, Row{}, fail(CodeMissing, "%s has no %s connection: connect it first", in.Principal, providerTitle(p, in.Provider))
	}
	if err != nil {
		return "", nil, Row{}, fail(CodeStore, "%v", err)
	}
	if row.Status == StatusReconnect {
		return "", nil, Row{}, fail(CodeReconnect, "%s's %s connection needs the person again: reconnect it", in.Principal, providerTitle(p, in.Provider))
	}
	return declStack, p, row, nil
}

// UseResult is the header to set: the consumer's declared header and the
// token in its declared format. Token is the bare token, for an op that
// gives a format of its own; it goes on the request and nowhere else.
type UseResult struct {
	Header string
	Value  string
	Token  string
	Row    Row
}

// Use decides whether this request may carry the principal's token at this
// provider, renews the token when due, and returns the header. Four facts
// must agree (docs/todo-connections.md §4): the pinned principal and
// capability, the executing stack as a consumer the provider names, and
// the destination in both declarations.
func (s *Service) Use(ctx context.Context, in UseInput) (UseResult, *Error) {
	if in.Principal == "" || in.Capability == "" {
		return UseResult{}, fail(CodeDenied, "no principal and capability are pinned on this call: a connection is used inside a capability call")
	}
	if !ValidName(in.Provider) {
		return UseResult{}, fail(CodeInvalidArg, "connection %q is not a provider name", in.Provider)
	}
	stack := baseStack(in.Stack)
	declStack, p, err := s.decls.Provider(ctx, in.Tenant, in.Provider)
	if errors.Is(err, ErrNotDeclared) {
		return UseResult{}, fail(CodeDenied, "no active stack of this tenant declares provider %q", in.Provider)
	}
	if err != nil {
		return UseResult{}, fail(CodeUnavailable, "%v", err)
	}
	if !p.Allows(stack, in.Capability) {
		return UseResult{}, fail(CodeDenied, "provider %q does not name stack %q as a consumer for %s", in.Provider, stack, in.Capability)
	}
	c, err := s.decls.Consumer(ctx, in.Tenant, stack, in.Provider)
	if errors.Is(err, ErrNotDeclared) {
		return UseResult{}, fail(CodeDenied, "stack %q has no %s", stack, ConsumerPath(in.Provider))
	}
	if err != nil {
		return UseResult{}, fail(CodeUnavailable, "%v", err)
	}
	o, oerr := OriginOf(in.URL)
	if oerr != nil {
		return UseResult{}, fail(CodeDestination, "%v", oerr)
	}
	if !c.OriginAllowed(o) || !p.APIOrigin(o) {
		return UseResult{}, fail(CodeDestination, "%s is not a declared destination of this connection", o)
	}
	tenantID, err := s.slugToID(ctx, in.Tenant)
	if err != nil {
		return UseResult{}, fail(CodeUnavailable, "%v", err)
	}
	row, err := s.store.Get(ctx, tenantID, in.Principal, in.Provider, "")
	if errors.Is(err, ErrNotFound) {
		return UseResult{}, fail(CodeMissing, "%s has no %s connection: connect it first", in.Principal, providerTitle(p, in.Provider))
	}
	if err != nil {
		return UseResult{}, fail(CodeStore, "%v", err)
	}
	if row.Kind != c.Kind {
		return UseResult{}, fail(CodeMissing, "%s's %s connection is %s; this stack uses %s", in.Principal, providerTitle(p, in.Provider), row.Kind, c.Kind)
	}
	access, row, uerr := s.current(ctx, in.Tenant, declStack, p, row)
	if uerr != nil {
		return UseResult{}, uerr
	}
	value, err := secrets.Substitute(c.Use.Format, []byte(access))
	if err != nil {
		return UseResult{}, fail(CodeStore, "use.format: %v", err)
	}
	return UseResult{Header: c.Use.Header, Value: value, Token: access, Row: row}, nil
}

// ProviderDecl resolves a provider's declaration: the declaring stack and
// the declaration, or ErrNotDeclared. For the ops that must know who may
// list a provider's connections.
func (s *Service) ProviderDecl(ctx context.Context, tenant, name string) (string, *Provider, error) {
	if !ValidName(name) {
		return "", nil, ErrNotDeclared
	}
	return s.decls.Provider(ctx, tenant, name)
}

// HTTPClient is the client a request that carries a connection's token is
// made with: guarded, and following no redirect.
func (s *Service) HTTPClient(u *url.URL) *http.Client {
	o, err := OriginOf(u)
	if err != nil {
		return s.clients.strict
	}
	return s.clients.For(o)
}

// ── list, revoke ─────────────────────────────────────────────────────────

// List is a principal's connections, metadata only.
func (s *Service) List(ctx context.Context, tenantID, principal string) ([]Row, error) {
	return s.store.ListByPrincipal(ctx, tenantID, principal)
}

// ListProvider is every connection at a provider, metadata only.
func (s *Service) ListProvider(ctx context.Context, tenantID, provider string) ([]Row, error) {
	return s.store.ListByProvider(ctx, tenantID, provider)
}

// Revoke removes a connection and, when the provider has a revocation
// destination, tells it. It returns false when there was none.
func (s *Service) Revoke(ctx context.Context, tenant, tenantID, principal, provider string) (Row, bool, *Error) {
	if !ValidName(provider) {
		return Row{}, false, fail(CodeInvalidArg, "`provider` %q is not a provider name", provider)
	}
	row, err := s.store.Get(ctx, tenantID, principal, provider, "")
	if errors.Is(err, ErrNotFound) {
		return Row{}, false, nil
	}
	if err != nil {
		return Row{}, false, fail(CodeStore, "%v", err)
	}
	// The provider's declaration may be gone (an orphaned row): the row is
	// still removed; nothing is told.
	var (
		p         *Provider
		declStack string
	)
	if ds, pp, derr := s.decls.Provider(ctx, tenant, provider); derr == nil {
		declStack, p = ds, pp
	}
	var m Material
	if p != nil && p.Destinations.Revocation != "none" {
		m, _ = s.store.Open(ctx, row)
	}
	row, removed, err := s.store.Delete(ctx, tenantID, principal, provider, "")
	if err != nil {
		return Row{}, false, fail(CodeStore, "%v", err)
	}
	if removed && p != nil && p.Destinations.Revocation != "none" {
		if creds, cerr := s.creds(ctx, tenant, declStack, p); cerr == nil {
			if rerr := s.revokeAt(ctx, p, creds, m.Access, "access_token"); rerr != nil {
				s.log.Warn("connection: revocation at the provider failed", zap.String("provider", provider), zap.Error(rerr))
			}
		}
	}
	if removed {
		s.event(ctx, "connection.revoked", map[string]any{"principal": principal, "provider": provider, "account": row.Account})
	}
	return row, removed, nil
}

// SweepFlows is for a periodic sweeper: flows past expiry are deleted.
func (s *Service) SweepFlows(ctx context.Context) (int64, error) { return s.store.SweepFlows(ctx) }
