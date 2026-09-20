package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// identity.go holds the handler bodies for the stack-plane identity ops —
// the op-writable surface over chassis/authn:
//
//   txco://user/create        create a user and bind its email (idempotent
//                             on the email, per stack)
//   txco://user/get           read a user by `id` or `email`
//   txco://user/disable       disable a user, or re-enable with disabled=false
//   txco://credential/create  issue a credential that authenticates AS a
//                             principal; the password is returned ONCE
//   txco://credential/list    a principal's credentials (never a secret)
//   txco://credential/revoke  revoke one by `id`, or all of a `principal`'s
//                             `except` one — the second half of a rotation
//
// Scoping is trusted, never read from a mutable _txc.* field: the tenant
// from processor.TenantScope(ctx), and the CALLING STACK from
// processor.StackScope(ctx) — the deployed rule's own stack. Every row is
// stamped with that stack's base name, and only that stack may change a
// principal's rows afterwards (authn/owner.go); `user/get` alone is open to
// any stack in the tenant.
//
// Output lands under `into` (default `_user` / `_credential`); errors as
// `<into>.error.{code,message}` with a nil Go error, so authors branch with
// `WHEN ._user.error.code != ""` and the run continues.

type identityDeps struct {
	store *authn.Store // nil ⇒ txco_<family>_disabled
	// snap returns the mirror DB (dbcache snapshot) the tenant slug → id
	// lookup reads; it is always SQLite. nil ⇒ every call is no_tenant.
	snap func() *sql.DB
}

func identityErr(into, family, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", "txco_"+family+"_"+code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// identityStoreErr maps a store error onto the op's error code. Argument and
// ownership errors carry their own message; anything else is `store`.
func identityStoreErr(into, family string, err error) event.Payload {
	code := "store"
	switch {
	case errors.Is(err, authn.ErrInvalid):
		code = "invalid_arg"
	case errors.Is(err, authn.ErrNotFound):
		code = "not_found"
	case errors.Is(err, authn.ErrNotOwner):
		code = "not_owner"
	case errors.Is(err, authn.ErrBound):
		code = "identifier_bound"
	case errors.Is(err, authn.ErrDisabled):
		code = "user_disabled"
	case errors.Is(err, authn.ErrTooMany):
		code = "too_many"
	case errors.Is(err, authn.ErrNoStack):
		code = "no_stack"
	}
	return identityErr(into, family, code, authorMessage(err))
}

// authorMessage is a store error as a rule author should read it: without
// the Go package prefix, and without restating the code beside it.
func authorMessage(err error) string {
	msg := strings.TrimPrefix(err.Error(), "authn: ")
	return strings.TrimPrefix(msg, "invalid argument: ")
}

func identityOK(into string, result any) event.Payload {
	b, _ := json.Marshal(result)
	raw, _ := sjson.SetRaw(`{}`, into, string(b))
	return event.Payload{Raw: raw, Type: event.JSON}
}

// identityCall is what every handler starts from: the pinned tenant (as the
// tenant ID the identity tables key on), the calling stack, the WITH meta.
type identityCall struct {
	tenantID, stack string
	meta            []byte
	into, family    string
}

func (c identityCall) err(code, msg string) event.Payload {
	return identityErr(c.into, c.family, code, msg)
}
func (c identityCall) storeErr(err error) event.Payload {
	return identityStoreErr(c.into, c.family, err)
}
func (c identityCall) str(key string) string { return gjson.GetBytes(c.meta, key).String() }

func identityPrelude(ctx context.Context, d identityDeps, family string) (identityCall, event.Payload, bool) {
	meta := []byte(operation.MetaFromContext(ctx))
	c := identityCall{meta: meta, into: intoPath(meta, "_"+family), family: family, stack: processor.StackScope(ctx)}
	slug := processor.TenantScope(ctx)
	if slug == "" {
		return c, c.err("no_tenant", "no tenant in request scope"), false
	}
	if d.store == nil {
		return c, c.err("disabled", "no identity store on this node (auth.db failed to open at boot; see the chassis log)"), false
	}
	tenantID, err := lookupTenantID(ctx, d.snap, slug)
	if err != nil {
		return c, c.err("no_tenant", err.Error()), false
	}
	c.tenantID = tenantID
	return c, event.Payload{}, true
}

func stampOut(t time.Time) string { return t.UTC().Format(time.RFC3339) }

type userOut struct {
	ID            string `json:"id"`
	Principal     string `json:"principal"`
	DisplayName   string `json:"display_name"`
	Status        string `json:"status"`
	Email         string `json:"email,omitempty"`
	EmailVerified *bool  `json:"email_verified,omitempty"`
	CreatedBy     string `json:"created_by"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
	Created       *bool  `json:"created,omitempty"`
}

func newUserOut(u authn.User, b *authn.Binding) userOut {
	out := userOut{
		ID: u.ID, Principal: u.Principal().ID, DisplayName: u.DisplayName, Status: u.Status,
		CreatedBy: u.CreatedBy, CreatedAt: stampOut(u.CreatedAt), UpdatedAt: stampOut(u.UpdatedAt),
	}
	if b != nil {
		out.Email, out.EmailVerified = b.Subject, &b.Verified
	}
	return out
}

// userCreate: WITH email (required), display_name, verified. Result at
// `into`: the user, its `principal`, and `created` (false when the email was
// already this stack's user — the op is safe to retry).
func userCreate(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "user")
	if !ok {
		return ep, nil
	}
	u, b, created, err := d.store.CreateUser(ctx, c.tenantID, c.stack, authn.NewUser{
		Email:         c.str("email"),
		EmailVerified: gjson.GetBytes(c.meta, "verified").Bool(),
		DisplayName:   c.str("display_name"),
	})
	if err != nil {
		return c.storeErr(err), nil
	}
	out := newUserOut(u, &b)
	out.Created = &created
	return identityOK(c.into, out), nil
}

// userGet: WITH id | email. Open to any stack in the tenant.
func userGet(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "user")
	if !ok {
		return ep, nil
	}
	id, email := c.str("id"), c.str("email")
	switch {
	case id != "" && email != "":
		return c.err("invalid_arg", "pass `id` or `email`, not both"), nil
	case id != "":
		u, err := d.store.GetUser(ctx, c.tenantID, id)
		if err != nil {
			return c.storeErr(err), nil
		}
		return identityOK(c.into, newUserOut(u, nil)), nil
	case email != "":
		u, b, err := d.store.UserByEmail(ctx, c.tenantID, email)
		if err != nil {
			return c.storeErr(err), nil
		}
		return identityOK(c.into, newUserOut(u, &b)), nil
	}
	return c.err("invalid_arg", "`id` or `email` is required"), nil
}

// userDisable: WITH id, disabled (default true; false re-enables). Nothing is
// revoked, so re-enabling restores the user's access.
func userDisable(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "user")
	if !ok {
		return ep, nil
	}
	id := c.str("id")
	if id == "" {
		return c.err("invalid_arg", "`id` is required"), nil
	}
	disabled := true
	if v := gjson.GetBytes(c.meta, "disabled"); v.Exists() {
		disabled = v.Bool()
	}
	u, err := d.store.SetUserDisabled(ctx, c.tenantID, c.stack, id, disabled)
	if err != nil {
		return c.storeErr(err), nil
	}
	return identityOK(c.into, newUserOut(u, nil)), nil
}

type credentialOut struct {
	ID         string   `json:"id"`
	ShortID    string   `json:"short_id"`
	Principal  string   `json:"principal"`
	Kind       string   `json:"kind"`
	Scopes     []string `json:"scopes"`
	Label      string   `json:"label"`
	CreatedBy  string   `json:"created_by"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt string   `json:"last_used_at,omitempty"`
	RevokedAt  string   `json:"revoked_at,omitempty"`
	Password   string   `json:"password,omitempty"`
}

func newCredentialOut(cr authn.Credential) credentialOut {
	out := credentialOut{
		ID: cr.ID, ShortID: cr.ShortID, Principal: cr.Principal.ID, Kind: string(cr.Kind),
		Scopes: cr.Scopes.Strings(), Label: cr.Label, CreatedBy: cr.CreatedBy, CreatedAt: stampOut(cr.CreatedAt),
	}
	if cr.LastUsedAt != nil {
		out.LastUsedAt = stampOut(*cr.LastUsedAt)
	}
	if cr.RevokedAt != nil {
		out.RevokedAt = stampOut(*cr.RevokedAt)
	}
	return out
}

// principalParam reads and validates the `principal` WITH param.
func (c identityCall) principalParam() (authn.Principal, event.Payload, bool) {
	raw := c.str("principal")
	if raw == "" {
		return authn.Principal{}, c.err("invalid_arg", "`principal` is required (user:usr_… from txco://user/create, or <kind>:<name>)"), false
	}
	p, err := authn.ParsePrincipal(raw)
	if err != nil {
		return authn.Principal{}, c.err("invalid_arg", err.Error()), false
	}
	return p, event.Payload{}, true
}

// stringsParam reads a WITH param that is a list of strings, or one string.
func (c identityCall) stringsParam(key string) []string {
	v := gjson.GetBytes(c.meta, key)
	switch {
	case v.IsArray():
		var out []string
		v.ForEach(func(_, item gjson.Result) bool { out = append(out, item.String()); return true })
		return out
	case v.Exists() && v.String() != "":
		return []string{v.String()}
	}
	return nil
}

// credentialCreate: WITH principal, scopes[], label, password_style
// (token|words), password_words. Result at `into`: the credential and its
// `password` — shown once; only its hash is stored. Every password is
// generated: it carries the credential's short id, so a caller cannot
// choose one.
func credentialCreate(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "credential")
	if !ok {
		return ep, nil
	}
	p, ep, ok := c.principalParam()
	if !ok {
		return ep, nil
	}
	if gjson.GetBytes(c.meta, "password").Exists() {
		return c.err("invalid_arg", "`password` is not accepted: every credential's password is generated, because it carries the credential's id"), nil
	}
	cr, password, err := d.store.IssueCredential(ctx, c.tenantID, c.stack, p, authn.NewCredential{
		Scopes: c.stringsParam("scopes"),
		Label:  c.str("label"),
		Style:  authn.PasswordStyle(c.str("password_style")),
		Words:  int(gjson.GetBytes(c.meta, "password_words").Int()),
	})
	if err != nil {
		return c.storeErr(err), nil
	}
	// Shown once, to the rule that asked — and scrubbed from every trace
	// record of this request (processor.ScrubbingTracer).
	processor.NoteIssuedSecret(ctx, password)
	out := newCredentialOut(cr)
	out.Password = password
	return identityOK(c.into, out), nil
}

// credentialList: WITH principal, include_revoked. Result: {principal, count,
// items[]}, newest first. The owning stack only.
func credentialList(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "credential")
	if !ok {
		return ep, nil
	}
	p, ep, ok := c.principalParam()
	if !ok {
		return ep, nil
	}
	list, err := d.store.ListCredentials(ctx, c.tenantID, c.stack, p, gjson.GetBytes(c.meta, "include_revoked").Bool())
	if err != nil {
		return c.storeErr(err), nil
	}
	items := make([]credentialOut, 0, len(list))
	for _, cr := range list {
		items = append(items, newCredentialOut(cr))
	}
	return identityOK(c.into, map[string]any{"principal": p.ID, "count": len(items), "items": items}), nil
}

// credentialRevoke revokes one credential, or rotates a principal's:
//
//	WITH id                       revoke one → {id, principal, revoked}
//	                              (revoked=false: it already was)
//	WITH id, principal            revoke that one ONLY IF it is <principal>'s;
//	                              anyone else's id is not_found. For an id
//	                              that came from a request.
//	WITH principal, except = <id> revoke every live credential but <id> →
//	                              {principal, revoked_count}
//	WITH principal, all = true    revoke every live credential
//
// A rotation is credential/create, then revoke with `except` = the new id.
// `except` must name a live credential of that principal, and a bare
// `principal` is refused: if the create step failed, its id is missing, and
// the op must not answer that by revoking the only password that works.
func credentialRevoke(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "credential")
	if !ok {
		return ep, nil
	}
	id, principal := c.str("id"), c.str("principal")
	switch {
	case id != "" && principal != "":
		// The id names the credential, the principal names whose it must be.
		// `except` and `all` are the OTHER meaning of `principal`; mixing the
		// two is ambiguous, so it stays an error.
		if c.str("except") != "" || gjson.GetBytes(c.meta, "all").Exists() {
			return c.err("invalid_arg", "with `id`, `principal` only checks whose credential it is: drop `except` and `all`"), nil
		}
		p, ep, ok := c.principalParam()
		if !ok {
			return ep, nil
		}
		cr, revoked, err := d.store.RevokeCredentialOf(ctx, c.tenantID, c.stack, p, id)
		if err != nil {
			return c.storeErr(err), nil
		}
		return identityOK(c.into, map[string]any{"id": cr.ID, "principal": cr.Principal.ID, "revoked": revoked}), nil
	case id != "":
		cr, revoked, err := d.store.RevokeCredential(ctx, c.tenantID, c.stack, id)
		if err != nil {
			return c.storeErr(err), nil
		}
		return identityOK(c.into, map[string]any{"id": cr.ID, "principal": cr.Principal.ID, "revoked": revoked}), nil
	case principal != "":
		p, ep, ok := c.principalParam()
		if !ok {
			return ep, nil
		}
		except, all := c.str("except"), gjson.GetBytes(c.meta, "all").Bool()
		if (except == "") == !all {
			return c.err("invalid_arg", "with `principal`, pass `except` = the credential id to keep, or `all` = true — one of them"), nil
		}
		n, err := d.store.RevokeCredentials(ctx, c.tenantID, c.stack, p, except)
		if err != nil {
			return c.storeErr(err), nil
		}
		return identityOK(c.into, map[string]any{"principal": p.ID, "revoked_count": n}), nil
	}
	return c.err("invalid_arg", "`id` or `principal` is required"), nil
}
