package server

import (
	"context"
	"errors"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/connection"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// connection.go holds the handler bodies for the connection ops — the
// op-writable surface over chassis/connection, a principal's identities at
// outside services:
//
//	txco://connection/start   begin a sign-in for a principal: the provider's
//	                          sign-in URL, and the flow cookie on the response
//	txco://connection/finish  on the callback: consume the flow, exchange the
//	                          code, store the tokens (never answered)
//	txco://connection/list    a principal's connections, or a provider's —
//	                          metadata only
//	txco://connection/revoke  remove one, and tell the provider when it has
//	                          a revocation destination
//
// Scoping is the identity ops' (identity.go): the tenant from the pinned
// scope, the calling stack from the dispatching rule, and only the stack
// that manages a principal may connect or disconnect it. `finish` reads
// the code, state and cookie from the HTTP request it is serving
// (`_txc.web.req`, which no rule can write), never from a WITH clause.
//
// Output lands under `into` (default `_connection`); errors as
// `<into>.error.{code,message}` with a nil Go error.

type connectionDeps struct {
	identityDeps
	svc *connection.Service // nil ⇒ txco_connection_disabled
}

type connectionOut struct {
	ID               string   `json:"id"`
	Principal        string   `json:"principal"`
	Provider         string   `json:"provider"`
	Label            string   `json:"label,omitempty"`
	Kind             string   `json:"kind"`
	Subject          string   `json:"subject,omitempty"`
	Account          string   `json:"account,omitempty"`
	Status           string   `json:"status"`
	Scopes           []string `json:"scopes"`
	Generation       int64    `json:"generation"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
	RefreshExpiresAt string   `json:"refresh_expires_at,omitempty"`
	CreatedBy        string   `json:"created_by"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
	Revoked          *bool    `json:"revoked,omitempty"`
}

func newConnectionOut(r connection.Row) connectionOut {
	out := connectionOut{
		ID: r.ID, Principal: r.PrincipalID, Provider: r.Provider, Label: r.Label, Kind: r.Kind,
		Subject: r.Subject, Account: r.Account, Status: r.Status, Scopes: r.Scopes, Generation: r.Generation,
		CreatedBy: r.CreatedBy, CreatedAt: stampOut(r.CreatedAt), UpdatedAt: stampOut(r.UpdatedAt),
	}
	if out.Scopes == nil {
		out.Scopes = []string{}
	}
	if r.ExpiresAt != nil {
		out.ExpiresAt = stampOut(*r.ExpiresAt)
	}
	if r.RefreshExpiresAt != nil {
		out.RefreshExpiresAt = stampOut(*r.RefreshExpiresAt)
	}
	return out
}

// connectionPrelude is identityPrelude plus the service check.
func connectionPrelude(ctx context.Context, d connectionDeps) (identityCall, event.Payload, bool) {
	c, ep, ok := identityPrelude(ctx, d.identityDeps, "connection")
	if !ok {
		return c, ep, false
	}
	if d.svc == nil {
		return c, c.err("disabled", "connections are not configured on this node (no master key, or the identity store failed to open; see the chassis log)"), false
	}
	return c, event.Payload{}, true
}

// webRequest is what finish and start read of the request they serve.
type webRequest struct {
	src, host, path string
	in              []byte
}

func readWebRequest(in []byte) webRequest {
	f := gjson.GetManyBytes(in, "_txc.src", "_txc.web.req.host", "_txc.web.req.url.path")
	return webRequest{src: f[0].String(), host: f[1].String(), path: f[2].String(), in: in}
}

func (w webRequest) query(k string) string {
	return gjson.GetBytes(w.in, "_txc.web.req.url.query."+k+".0").String()
}

func (w webRequest) cookie(name string) string {
	return gjson.GetBytes(w.in, "_txc.web.req.cookies."+name+".0").String()
}

// withCookie adds a Set-Cookie line to a payload (the merge appends it to
// any the response already has).
func withCookie(p event.Payload, line string) event.Payload {
	if line == "" {
		return p
	}
	raw, err := sjson.Set(p.Raw, "_txc.web.res.headers.set-cookie.0", line)
	if err == nil {
		p.Raw = raw
	}
	return p
}

// connectionStart: WITH principal, provider, redirect_uri [, kind, initiator].
// On the HTTP request that will carry the cookie (@src == "http"). Result
// at `into`: {url, provider, principal, expires_at} — the rule redirects
// the browser to `url`. The managing stack only.
func connectionStart(ctx context.Context, d connectionDeps, in []byte) (event.Payload, error) {
	c, ep, ok := connectionPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	p, ep, ok := c.principalParam()
	if !ok {
		return ep, nil
	}
	base, err := d.store.RequireOwner(ctx, c.tenantID, c.stack, p)
	if err != nil {
		return c.storeErr(err), nil
	}
	req := readWebRequest(in)
	if req.src != "http" || req.host == "" {
		return c.err("invalid_arg", "connection/start runs on the HTTP request whose response sets the browser's cookie (@src == \"http\")"), nil
	}
	res, cerr := d.svc.Start(ctx, connection.StartInput{
		Tenant: c.tenant, TenantID: c.tenantID, Stack: base, Principal: p.ID,
		Provider: c.str("provider"), Kind: c.str("kind"), Label: c.str("label"),
		RedirectURI: c.str("redirect_uri"), Host: req.host, Initiator: c.str("initiator"),
	})
	if cerr != nil {
		return c.err(cerr.Code, cerr.Message), nil
	}
	out := identityOK(c.into, map[string]any{
		"url": res.URL, "provider": c.str("provider"), "principal": p.ID, "expires_at": stampOut(res.ExpiresAt),
	})
	return withCookie(out, res.Cookie), nil
}

// connectionFinish: no WITH but `into`. On the callback request: the code,
// state, iss and cookie are read from `_txc.web.req`. Result at `into`: the
// connection's metadata. The stack that started the flow only.
func connectionFinish(ctx context.Context, d connectionDeps, in []byte) (event.Payload, error) {
	c, ep, ok := connectionPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	req := readWebRequest(in)
	if req.src != "http" || req.host == "" {
		return c.err("invalid_arg", "connection/finish runs on the callback request itself (@src == \"http\"): it reads code, state and the cookie from it"), nil
	}
	res, cerr := d.svc.Finish(ctx, connection.FinishInput{
		Tenant: c.tenant, TenantID: c.tenantID, Stack: c.stack, Host: req.host, Path: req.path,
		Code: req.query("code"), State: req.query("state"), Iss: req.query("iss"), ProviderError: req.query("error"),
		Cookie: req.cookie(connection.CookieName),
	})
	if cerr != nil {
		return withCookie(c.err(cerr.Code, cerr.Message), res.ClearCookie), nil
	}
	return withCookie(identityOK(c.into, newConnectionOut(res.Row)), res.ClearCookie), nil
}

// connectionList lists connections one of two ways:
//
//	WITH principal   what this principal has → {principal, count, items[]}.
//	                 The managing stack only.
//	WITH provider    who is connected at this provider → {provider, count,
//	                 items[]}. The provider's declaring stack and the
//	                 stacks it names as consumers.
//
// Metadata only: never a token.
func connectionList(ctx context.Context, d connectionDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := connectionPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	principal, provider := c.str("principal"), c.str("provider")
	switch {
	case principal != "" && provider != "":
		return c.err("invalid_arg", "pass `principal` to list what it has, or `provider` to list who is connected there — not both"), nil
	case principal != "":
		p, ep, ok := c.principalParam()
		if !ok {
			return ep, nil
		}
		if _, err := d.store.RequireOwner(ctx, c.tenantID, c.stack, p); err != nil {
			return c.storeErr(err), nil
		}
		rows, err := d.svc.List(ctx, c.tenantID, p.ID)
		if err != nil {
			return c.err("store", err.Error()), nil
		}
		return identityOK(c.into, map[string]any{"principal": p.ID, "count": len(rows), "items": outs(rows)}), nil
	case provider != "":
		declStack, decl, err := d.svc.ProviderDecl(ctx, c.tenant, provider)
		if errors.Is(err, connection.ErrNotDeclared) {
			return c.err("not_declared", "no active stack of this tenant declares provider "+provider), nil
		}
		if err != nil {
			return c.err("store", err.Error()), nil
		}
		base := baseStackOf(c.stack)
		if base != declStack && decl.Consumers[base] == nil {
			return c.err("not_owner", "provider "+provider+" is declared by stack "+declStack+", which names "+base+" as no consumer"), nil
		}
		rows, err := d.svc.ListProvider(ctx, c.tenantID, provider)
		if err != nil {
			return c.err("store", err.Error()), nil
		}
		return identityOK(c.into, map[string]any{"provider": provider, "count": len(rows), "items": outs(rows)}), nil
	}
	return c.err("invalid_arg", "`principal` or `provider` is required"), nil
}

func outs(rows []connection.Row) []connectionOut {
	items := make([]connectionOut, 0, len(rows))
	for _, r := range rows {
		items = append(items, newConnectionOut(r))
	}
	return items
}

// baseStackOf reduces a dispatching stack to the one that owns rows (its
// first segment), as authn.BaseStack does.
func baseStackOf(stack string) string {
	for i := 0; i < len(stack); i++ {
		if stack[i] == '/' {
			return stack[:i]
		}
	}
	return stack
}

// connectionRevoke: WITH principal, provider. Result: the connection that
// was removed and `revoked` (false: there was none). The managing stack
// only. A run using it is refused at its next request.
func connectionRevoke(ctx context.Context, d connectionDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := connectionPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	p, ep, ok := c.principalParam()
	if !ok {
		return ep, nil
	}
	provider := c.str("provider")
	if provider == "" {
		return c.err("invalid_arg", "`provider` is required"), nil
	}
	if _, err := d.store.RequireOwner(ctx, c.tenantID, c.stack, p); err != nil {
		return c.storeErr(err), nil
	}
	row, removed, cerr := d.svc.Revoke(ctx, c.tenant, c.tenantID, p.ID, provider)
	if cerr != nil {
		return c.err(cerr.Code, cerr.Message), nil
	}
	if !removed {
		return identityOK(c.into, map[string]any{"principal": p.ID, "provider": provider, "revoked": false}), nil
	}
	out := newConnectionOut(row)
	out.Revoked = &removed
	return identityOK(c.into, out), nil
}
