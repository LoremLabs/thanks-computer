package server

import (
	"context"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// grant.go holds the handler bodies for the standing-grant ops — the
// op-writable surface over chassis/authn's resource_grants:
//
//   txco://grant/put     grant a principal a capability or a secret
//                        (idempotent)
//   txco://grant/list    a principal's grants, or the grants on a resource
//   txco://grant/revoke  end one, by id or by what it names
//
// A standing grant says what a principal may EVER ask for. It decides
// nothing on its own: a run grant (delegate.go) names what one piece of work
// may ask for, and can name only what its principal holds here.
//
// Scoping is the identity ops' (identity.go): the tenant from the pinned
// scope, the calling stack from the dispatching rule, and only the stack
// that manages a principal may grant to it.
//
// Output lands under `into` (default `_grant`); errors as
// `<into>.error.{code,message}` with a nil Go error.

type grantOut struct {
	ID        string   `json:"id"`
	Principal string   `json:"principal"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Verbs     []string `json:"verbs"`
	CreatedBy string   `json:"created_by"`
	CreatedAt string   `json:"created_at"`
	RevokedAt string   `json:"revoked_at,omitempty"`
	Created   *bool    `json:"created,omitempty"`
	Revoked   *bool    `json:"revoked,omitempty"`
}

func newGrantOut(g authn.Grant) grantOut {
	out := grantOut{
		ID: g.ID, Principal: g.Principal.ID, Kind: string(g.Resource.Kind), Name: g.Resource.Name,
		Verbs: g.Verbs, CreatedBy: g.CreatedBy, CreatedAt: stampOut(g.CreatedAt),
	}
	if g.RevokedAt != nil {
		out.RevokedAt = stampOut(*g.RevokedAt)
	}
	return out
}

// resourceParams reads the `kind` and `name` WITH params. It checks only
// that both are there; the store owns their grammar.
func (c identityCall) resourceParams() (authn.ResourceKind, string, event.Payload, bool) {
	kind, name := c.str("kind"), c.str("name")
	if kind == "" || name == "" {
		return "", "", c.err("invalid_arg", "`kind` (capability or secret) and `name` are required"), false
	}
	return authn.ResourceKind(kind), name, event.Payload{}, true
}

// grantPut: WITH principal, kind, name, verbs[]. Result at `into`: the grant,
// and `created` (false when it already held — the op is safe to retry).
func grantPut(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "grant")
	if !ok {
		return ep, nil
	}
	p, ep, ok := c.principalParam()
	if !ok {
		return ep, nil
	}
	kind, name, ep, ok := c.resourceParams()
	if !ok {
		return ep, nil
	}
	g, created, err := d.store.PutGrant(ctx, c.tenantID, c.stack, p, authn.NewGrant{
		Kind: kind, Name: name, Verbs: c.stringsParam("verbs"),
	})
	if err != nil {
		return c.storeErr(err), nil
	}
	out := newGrantOut(g)
	out.Created = &created
	return identityOK(c.into, out), nil
}

// grantList lists grants one of two ways:
//
//	WITH principal [, kind] [, include_revoked]  what this principal holds →
//	                                             {principal, count, items[]}.
//	                                             The owning stack only.
//	WITH kind, name                              who holds this resource →
//	                                             {kind, name, count, items[]}.
//	                                             Open to any stack in the
//	                                             tenant; live grants only.
func grantList(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "grant")
	if !ok {
		return ep, nil
	}
	var (
		list []authn.Grant
		err  error
		head = map[string]any{}
	)
	if c.str("principal") != "" {
		if c.str("name") != "" {
			return c.err("invalid_arg", "pass `principal` to list what it holds, or `kind` and `name` to list who holds a resource — not both"), nil
		}
		p, ep, ok := c.principalParam()
		if !ok {
			return ep, nil
		}
		list, err = d.store.ListGrants(ctx, c.tenantID, c.stack, p, authn.ResourceKind(c.str("kind")),
			gjson.GetBytes(c.meta, "include_revoked").Bool())
		head["principal"] = p.ID
	} else {
		kind, name, _, ok := c.resourceParams()
		if !ok {
			return c.err("invalid_arg", "`principal`, or `kind` and `name`, is required"), nil
		}
		list, err = d.store.GrantsOn(ctx, c.tenantID, kind, name)
		head["kind"], head["name"] = string(kind), name
	}
	if err != nil {
		return c.storeErr(err), nil
	}
	items := make([]grantOut, 0, len(list))
	for _, g := range list {
		items = append(items, newGrantOut(g))
	}
	head["count"], head["items"] = len(items), items
	return identityOK(c.into, head), nil
}

// grantRevoke ends one grant:
//
//	WITH id                     by id → the grant, and `revoked` (false: it
//	                            already was)
//	WITH id, principal          that one ONLY IF it is <principal>'s; anyone
//	                            else's id is not_found. For an id that came
//	                            from a request.
//	WITH principal, kind, name  by what it names, the way it was put
//
// A run that leans on the grant is refused at its next request.
func grantRevoke(ctx context.Context, d identityDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d, "grant")
	if !ok {
		return ep, nil
	}
	var (
		g       authn.Grant
		revoked bool
		err     error
	)
	id := c.str("id")
	switch {
	case id != "" && (c.str("kind") != "" || c.str("name") != ""):
		return c.err("invalid_arg", "pass `id`, or `principal` with `kind` and `name` — not both"), nil
	case id != "" && c.str("principal") != "":
		p, ep, ok := c.principalParam()
		if !ok {
			return ep, nil
		}
		g, revoked, err = d.store.RevokeGrantOf(ctx, c.tenantID, c.stack, p, id)
	case id != "":
		g, revoked, err = d.store.RevokeGrant(ctx, c.tenantID, c.stack, id)
	case c.str("principal") == "":
		return c.err("invalid_arg", "`id`, or `principal` with `kind` and `name`, is required"), nil
	default:
		p, ep, ok := c.principalParam()
		if !ok {
			return ep, nil
		}
		kind, name, ep, ok := c.resourceParams()
		if !ok {
			return ep, nil
		}
		g, revoked, err = d.store.RevokeGrantOn(ctx, c.tenantID, c.stack, p, kind, name)
	}
	if err != nil {
		return c.storeErr(err), nil
	}
	out := newGrantOut(g)
	out.Revoked = &revoked
	return identityOK(c.into, out), nil
}
