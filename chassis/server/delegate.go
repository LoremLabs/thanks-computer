package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// delegate.go holds the handler bodies for the delegate ops — the
// op-writable surface over chassis/authn's run_grants. To delegate is to
// hand part of what a principal may ask for to ONE piece of its work; the
// record of it is a run grant.
//
//   txco://delegate/mint    write the run grant for one piece of work,
//                           when it is dispatched
//   txco://delegate/get     read one: what it may ask for, what it has spent
//   txco://delegate/revoke  end one now; the work's next request is refused
//   txco://delegate/close   end one because its work ended
//
// A run grant says what ONE piece of work may ask this chassis for, until
// when, and within what budget. It names SANDBOXES (chassis/sandbox): the
// minting stack's SANDBOXES/<name>.yaml files, each saying what a program
// that opens it is handed and under which variable names. Every secret they
// name must be one the principal holds a standing grant for (grant.go).
//
// None of these ops returns a token. The token that travels with the work
// is signed by the chassis at the moment it hands the work over, and never
// enters an envelope: a rule holds the grant's id, which identifies the row
// and authorizes nothing.
//
// Scoping is the identity ops' (identity.go). The stack that mints a grant
// is the only one that may read, revoke or close it.
//
// Output lands under `into` (default `_delegate`); errors as
// `<into>.error.{code,message}` with a nil Go error.

type runGrantDeps struct {
	identityDeps
	sandboxes                sandboxDecls
	ttlDefault, ttlMax       time.Duration
	budgetDefault, budgetMax int64
}

func newRunGrantDeps(d identityDeps, sandboxes sandboxDecls, conf config.Config) runGrantDeps {
	return runGrantDeps{
		identityDeps:  d,
		sandboxes:     sandboxes,
		ttlDefault:    time.Duration(conf.RunGrantTTLDefault) * time.Second,
		ttlMax:        time.Duration(conf.RunGrantTTLMax) * time.Second,
		budgetDefault: int64(conf.RunGrantBudgetDefault),
		budgetMax:     int64(conf.RunGrantBudgetMax),
	}
}

type callsOut struct {
	Calls int64 `json:"calls"`
}

type runGrantOut struct {
	ID          string              `json:"id"`
	Principal   string              `json:"principal"`
	Run         string              `json:"run"`
	Generation  int64               `json:"generation"`
	Workspace   string              `json:"workspace,omitempty"`
	WorkspaceID string              `json:"workspace_id,omitempty"`
	NodeClass   string              `json:"node_class"`
	Allow       []string            `json:"allow"`
	Sandboxes   map[string][]string `json:"sandboxes"`
	Budget      callsOut            `json:"budget"`
	Spent       callsOut            `json:"spent"`
	Parent      string              `json:"parent,omitempty"`
	Depth       int                 `json:"depth"`
	MintedBy    string              `json:"minted_by"`
	IssuedAt    string              `json:"issued_at"`
	ExpiresAt   string              `json:"expires_at"`
	ClosedAt    string              `json:"closed_at,omitempty"`
	CloseReason string              `json:"close_reason,omitempty"`
	RevokedAt   string              `json:"revoked_at,omitempty"`
	Live        bool                `json:"live"`
	Revoked     *bool               `json:"revoked,omitempty"`
	Closed      *bool               `json:"closed,omitempty"`
}

// newRunGrantOut is the row as a rule may see it. The file key is left out:
// it is for the chassis alone. `allow` is the sandboxes' secrets; `sandboxes`
// is each sandbox's variable names, never a value.
func newRunGrantOut(g authn.RunGrant, now time.Time) runGrantOut {
	out := runGrantOut{
		ID: g.ID, Principal: g.Principal.ID, Run: g.Run, Generation: g.Generation,
		Workspace: g.Workspace, WorkspaceID: g.WorkspaceID, NodeClass: string(g.NodeClass),
		Allow: g.AllowStrings(), Sandboxes: map[string][]string{},
		Budget: callsOut{g.BudgetCalls}, Spent: callsOut{g.SpentCalls},
		Parent: g.ParentGrant, Depth: g.Depth, MintedBy: g.MintedBy,
		IssuedAt: stampOut(g.IssuedAt), ExpiresAt: stampOut(g.ExpiresAt),
		CloseReason: g.CloseReason, Live: g.Live(now),
	}
	for _, name := range g.SandboxNames() {
		env := g.Sandboxes[name]
		vars := make([]string, 0, len(env))
		for v := range env {
			vars = append(vars, v)
		}
		sort.Strings(vars)
		out.Sandboxes[name] = vars
	}
	if g.ClosedAt != nil {
		out.ClosedAt = stampOut(*g.ClosedAt)
	}
	if g.RevokedAt != nil {
		out.RevokedAt = stampOut(*g.RevokedAt)
	}
	return out
}

func runGrantStoreErr(c identityCall, err error) event.Payload {
	code := ""
	switch {
	case errors.Is(err, authn.ErrExceedsStanding):
		code = "exceeds_standing"
	case errors.Is(err, authn.ErrExceedsParent):
		code = "exceeds_parent"
	case errors.Is(err, authn.ErrParentNotLive):
		code = "parent_not_live"
	case errors.Is(err, authn.ErrDepth):
		code = "depth"
	case errors.Is(err, authn.ErrStaleGeneration):
		code = "stale_generation"
	default:
		return c.storeErr(err)
	}
	return c.err(code, authorMessage(err))
}

// wholeParam reads an optional WITH param that must be a whole number from
// lo to hi; absent, it is dflt.
func (c identityCall) wholeParam(key, unit string, dflt, lo, hi int64) (int64, event.Payload, bool) {
	v := gjson.GetBytes(c.meta, key)
	if !v.Exists() {
		return dflt, event.Payload{}, true
	}
	n := v.Int()
	if v.Type != gjson.Number || float64(n) != v.Float() || n < lo || n > hi {
		return 0, c.err("invalid_arg", fmt.Sprintf("`%s` is %s, a whole number from %d to %d", key, unit, lo, hi)), false
	}
	return n, event.Payload{}, true
}

// delegateMint: WITH principal, run, allow[], and optionally workspace,
// node_class (reviewed|unreviewed), ttl (seconds), budget (requests),
// generation, parent (a run grant id to narrow from). Result at `into`: the
// grant.
//
// `allow` names the SANDBOXES the work may open: each is a
// SANDBOXES/<name>.yaml of the calling app stack, read from its active
// version now and copied into the grant. The principal must hold a standing
// grant for every secret they name.
//
// `workspace` is a workspace of the CALLING stack, by name — the one the
// work is about to be sent to with `workspace://<name>/exec WITH grant`. It
// need not exist yet.
//
// Minting a `run` that already has a grant closes the older one.
func delegateMint(ctx context.Context, d runGrantDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d.identityDeps, "delegate")
	if !ok {
		return ep, nil
	}
	p, ep, ok := c.principalParam()
	if !ok {
		return ep, nil
	}
	if gjson.GetBytes(c.meta, "token").Exists() || gjson.GetBytes(c.meta, "file_key").Exists() {
		return c.err("invalid_arg", "`token` and `file_key` are not accepted: the chassis makes both"), nil
	}
	ttl, ep, ok := c.wholeParam("ttl", "seconds", int64(d.ttlDefault/time.Second), 1, int64(d.ttlMax/time.Second))
	if !ok {
		return ep, nil
	}
	budget, ep, ok := c.wholeParam("budget", "requests", d.budgetDefault, 1, d.budgetMax)
	if !ok {
		return ep, nil
	}
	generation, ep, ok := c.wholeParam("generation", "the run's generation", 0, 1, 1<<53)
	if !ok {
		return ep, nil
	}
	in := authn.NewRunGrant{
		Run: c.str("run"), Generation: generation, Stack: workspace.AppStack(c.stack),
		NodeClass:   authn.NodeClass(c.str("node_class")),
		BudgetCalls: budget, TTL: time.Duration(ttl) * time.Second, Parent: c.str("parent"),
	}
	in.TraceID, _ = ctx.Value(config.CtxKeyRid).(string)
	if in.Stack == "" {
		return c.storeErr(authn.ErrNoStack), nil
	}
	// The sandboxes are the app stack's own — a canary slot declares its
	// own, as it does its outlets — read from its active version now.
	sandboxes, ep, ok := c.sandboxesParam(ctx, d.sandboxes, in.Stack)
	if !ok {
		return ep, nil
	}
	in.Sandboxes = sandboxes
	if name := c.str("workspace"); name != "" {
		if err := workspace.ValidateName(name); err != nil {
			return c.err("invalid_arg", err.Error()), nil
		}
		// The identity of a workspace is (tenant, app stack, name): naming
		// one here can only ever reach a workspace of the calling stack.
		in.Workspace, in.WorkspaceID = name, workspace.ID(c.tenant, in.Stack, name)
	}
	g, err := d.store.MintRunGrant(ctx, c.tenantID, c.stack, p, in)
	if err != nil {
		return runGrantStoreErr(c, err), nil
	}
	return identityOK(c.into, newRunGrantOut(g, g.IssuedAt)), nil
}

// sandboxesParam reads `allow`: the names of sandboxes the app stack
// declares, resolved through decls to what each sets. An unknown name is
// refused with the names the stack has.
func (c identityCall) sandboxesParam(ctx context.Context, decls sandboxDecls, stack string) (map[string]map[string]string, event.Payload, bool) {
	names := c.stringsParam("allow")
	if len(names) == 0 {
		return nil, c.err("invalid_arg", "`allow` is required: the sandboxes the work may open, by name (SANDBOXES/<name>.yaml of this stack)"), false
	}
	if len(names) > authn.MaxRunSandboxes {
		return nil, c.err("invalid_arg", fmt.Sprintf("`allow` names %d sandboxes, at most %d", len(names), authn.MaxRunSandboxes)), false
	}
	if decls == nil {
		return nil, c.err("no_sandbox", "this chassis has no sandbox declarations"), false
	}
	out := map[string]map[string]string{}
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if !sandbox.ValidName(name) {
			return nil, c.err("invalid_arg", fmt.Sprintf("`allow`: %q is not a sandbox name (a name matching [a-z][a-z0-9_-]*, at most 64 chars)", raw)), false
		}
		if _, dup := out[name]; dup {
			continue
		}
		d, err := decls.Lookup(ctx, c.tenant, stack, name)
		switch {
		case errors.Is(err, sandbox.ErrNotDeclared):
			have, _ := decls.Names(ctx, c.tenant, stack)
			msg := fmt.Sprintf("stack %s declares no sandbox %q", stack, name)
			if len(have) > 0 {
				msg += fmt.Sprintf(" (it has: %s)", strings.Join(have, ", "))
			} else {
				msg += " (it has none: add SANDBOXES/" + name + sandbox.DeclExt + " and apply)"
			}
			return nil, c.err("no_sandbox", msg), false
		case err != nil:
			return nil, c.err("store", err.Error()), false
		}
		env := make(map[string]string, len(d.Env))
		for v, ref := range d.Env {
			env[v] = ref
		}
		out[name] = env
	}
	return out, event.Payload{}, true
}

// idParam reads the required `id` WITH param.
func (c identityCall) idParam() (string, event.Payload, bool) {
	id := c.str("id")
	if id == "" {
		return "", c.err("invalid_arg", "`id` is required: the run grant's id, from txco://delegate/mint"), false
	}
	return id, event.Payload{}, true
}

// delegateGet: WITH id. Result: the grant, with what it has `spent` and
// whether it is still `live`. The minting stack only.
func delegateGet(ctx context.Context, d runGrantDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d.identityDeps, "delegate")
	if !ok {
		return ep, nil
	}
	id, ep, ok := c.idParam()
	if !ok {
		return ep, nil
	}
	g, err := d.store.GetRunGrant(ctx, c.tenantID, c.stack, id)
	if err != nil {
		return runGrantStoreErr(c, err), nil
	}
	return identityOK(c.into, newRunGrantOut(g, time.Now())), nil
}

// delegateRevoke: WITH id. Ends the grant and every grant narrowed from it;
// `revoked` is false when it already was. The work's next request is
// refused. Nothing the work was already given is recalled.
func delegateRevoke(ctx context.Context, d runGrantDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d.identityDeps, "delegate")
	if !ok {
		return ep, nil
	}
	id, ep, ok := c.idParam()
	if !ok {
		return ep, nil
	}
	g, revoked, err := d.store.RevokeRunGrant(ctx, c.tenantID, c.stack, id)
	if err != nil {
		return runGrantStoreErr(c, err), nil
	}
	out := newRunGrantOut(g, time.Now())
	out.Revoked = &revoked
	return identityOK(c.into, out), nil
}

// delegateClose: WITH id, reason. Ends the grant because its work ended,
// with every grant narrowed from it; `closed` is false when it had already
// ended. `reason` is a short note for whoever reads the row (default
// "done").
func delegateClose(ctx context.Context, d runGrantDeps, _ []byte) (event.Payload, error) {
	c, ep, ok := identityPrelude(ctx, d.identityDeps, "delegate")
	if !ok {
		return ep, nil
	}
	id, ep, ok := c.idParam()
	if !ok {
		return ep, nil
	}
	g, closed, err := d.store.CloseRunGrant(ctx, c.tenantID, c.stack, id, c.str("reason"))
	if err != nil {
		return runGrantStoreErr(c, err), nil
	}
	out := newRunGrantOut(g, time.Now())
	out.Closed = &closed
	return identityOK(c.into, out), nil
}
