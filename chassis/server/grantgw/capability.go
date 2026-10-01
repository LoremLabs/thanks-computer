package grantgw

import (
	"context"
	"encoding/json"
	"errors"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// A CAPABILITY is the other thing a run grant may name: not a secret handed
// over, but something the work may ask this chassis to DO — call a model,
// note a card, finish the run. It is named in a SANDBOXES/ declaration
// (`capabilities:`), copied into the grant's allowlist at mint, and decided
// here when the call is made, the way a secret is decided when a sandbox is
// opened:
//
//  1. HARD CHECKS: the token is genuine and unexpired, its row is live and
//     current, the tenant is live, the name is a capability's.
//  2. THE PROPOSAL: the run grant names the capability, the principal holds
//     a standing grant to invoke it, budget remains.
//  3. THE TENANT'S `_grant` STACK sees the request — kind `capability`, via
//     `http`, the call's input under `@grant.input` — and may allow, refuse
//     or HOLD it (`@grant.res.hold`: a person decides later).
//  4. THE VERDICT. Allowed: one request is charged to the grant, and the
//     caller (chassis/server/capgw) runs the stack that declares the
//     capability (CAPS/<name>.yaml), or the tenant's `_cap` stack.
//
// The chassis does not know what a capability does: that is the `_cap`
// stack's. It knows only whether this run may ask for it now.

// Call is one capability call made with a run grant.
type Call struct {
	Token string
	Name  string          // the capability, by its bare name
	Input json.RawMessage // what the caller sent, presented to the rules as data
}

// Verdict is what the inlet gets back: allowed, and then who is asking, or
// why not.
type Verdict struct {
	Allowed bool
	// Held: a rule held the call for a person to decide. Not a refusal to
	// the caller, which may end its run saying so.
	Held bool
	// Unavailable: the chassis could not decide.
	Unavailable bool
	// Reason is why not. For the log and for tests; the caller is told
	// only the class of the answer.
	Reason string
	// Grant and Tenant identify the run that asked, when the token was
	// genuine: what the `_cap` run is stamped with.
	Grant  authn.RunGrant
	Tenant string // slug
}

func (v Verdict) refused(reason string) Verdict {
	v.Allowed, v.Held, v.Unavailable, v.Reason = false, false, false, reason
	return v
}

func (v Verdict) unavailable(reason string) Verdict {
	v.Allowed, v.Held, v.Unavailable, v.Reason = false, false, true, reason
	return v
}

// Identified reports whether the token named a live grant: the hard
// checks passed, whatever was decided after. An inlet answers 401 when
// they did not.
func (v Verdict) Identified() bool { return v.Grant.ID != "" }

// Invoke decides one capability call and, when it is allowed, charges the
// grant one request. It runs nothing: the caller does, with the grant the
// verdict names.
func (g *Gateway) Invoke(ctx context.Context, c Call) Verdict {
	now := g.now()
	var v Verdict
	grant, ans, ok := g.identify(ctx, Open{Token: c.Token, Via: ViaHTTP}, now)
	if !ok {
		g.log.Info("grant: capability refused before any rule ran",
			zap.String("reason", ans.Reason), zap.Bool("unavailable", ans.Unavailable),
			zap.String("capability", c.Name), zap.String("grant", grant.ID))
		if ans.Unavailable {
			return v.unavailable(ans.Reason)
		}
		return v.refused(ans.Reason)
	}
	v.Grant = grant
	fields := []zap.Field{
		zap.String("grant", grant.ID), zap.String("run", grant.Run),
		zap.String("principal", grant.Principal.ID), zap.String("via", ViaHTTP),
		zap.String("capability", c.Name),
	}
	hard := func(v Verdict) Verdict {
		g.log.Info("grant: capability refused before any rule ran",
			append(fields, zap.String("reason", v.Reason), zap.Bool("unavailable", v.Unavailable))...)
		return v
	}
	res, err := authn.NewResource(authn.ResourceCapability, c.Name)
	if err != nil {
		return hard(v.refused(reasonName))
	}
	tenant, err := g.tenantSlug(ctx, grant.TenantID)
	switch {
	case errors.Is(err, errNoTenant):
		return hard(v.refused(reasonTenant))
	case err != nil:
		g.log.Warn("grant: reading the tenant failed", zap.Error(err))
		return hard(v.unavailable("tenant directory"))
	}
	v.Tenant = tenant
	fields = append(fields, zap.String("tenant", tenant))

	key := refusalKey(grant.ID, res)
	if reason, again := g.seen.get(key, now); again {
		g.log.Debug("grant: refused again, from memory", append(fields, zap.String("reason", reason))...)
		return v.refused(reason)
	}

	// The chassis's own reading of the call. `exists` and `pull` hold: the
	// allowlist, the standing grant and the budget decide. A stack declares
	// who ANSWERS a capability (CAPS/<name>.yaml, chassis/capdecl), and the
	// inlet routes by it, but `exists` is not read from the declarations
	// yet: an undeclared name still reaches the tenant's `_cap` stack, the
	// router written before declarations existed, and a tenant between the
	// two must not have every call refused. Once no tenant routes through
	// `_cap`, the inlet answers 404 for an undeclared name before it asks
	// here at all.
	standing, err := g.ids.Granted(ctx, grant.TenantID, grant.Principal, res, res.Verb())
	if err != nil {
		g.log.Warn("grant: reading the standing grant failed", append(fields, zap.Error(err))...)
		return v.unavailable("identity store")
	}
	ch := checks{
		exists:    true,
		allowlist: grant.Allows(res),
		standing:  standing,
		pull:      true,
		budget:    grant.Remaining() >= 1,
	}
	f := facts{
		rid: hxid.NewTimeSort().String(), tenant: tenant, grant: grant, res: res,
		rq: request{via: ViaHTTP}, input: c.Input, checks: ch, prop: propose(ch), at: now,
	}
	fields = append(fields, zap.String("rid", f.rid), zap.Bool("proposed", f.prop.allow))

	// The tenant's rules, as for a secret: a run of its own, off the
	// gateway's context, ended when the caller gives up or at the deadline.
	rctx, cancel := context.WithTimeout(g.ctx, g.timeout)
	rctx = context.WithValue(rctx, config.CtxKeyRid, f.rid)
	stop := context.AfterFunc(ctx, cancel)
	final, runErr := g.runStack(rctx, requestPayload(f))
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded)
	stop()
	cancel()
	if runErr != nil && !timedOut {
		g.log.Warn("grant: the _grant run failed", append(fields, zap.Error(runErr))...)
	}
	var verdictOf verdict
	if runErr == nil {
		verdictOf = parseVerdict(final)
	}
	d := decide(f.prop, verdictOf, runErr, timedOut)
	fields = append(fields, zap.Bool("by_rule", d.byRule))
	if d.note != "" {
		fields = append(fields, zap.String("note", d.note))
	}
	if !d.allow {
		if d.reason == reasonHeld {
			// A hold is a person's decision pending, not a fact about the
			// request: it is not remembered, so the same call asked again
			// after the person decided is decided afresh.
			g.log.Info("grant: capability held", append(fields, zap.String("reason", d.reason))...)
			v.Held, v.Reason = true, d.reason
			return v
		}
		g.seen.put(key, d.reason, now)
		g.log.Info("grant: capability refused", append(fields, zap.String("reason", d.reason))...)
		return v.refused(d.reason)
	}

	enforce := !(d.byRule && !ch.budget)
	switch err := g.ids.ChargeRunGrant(ctx, grant.TenantID, grant.ID, 1, enforce); {
	case errors.Is(err, authn.ErrRunBudget):
		g.seen.put(key, reasonBudget, now)
		g.log.Info("grant: capability refused", append(fields, zap.String("reason", reasonBudget))...)
		return v.refused(reasonBudget)
	case errors.Is(err, authn.ErrRunNotLive), errors.Is(err, authn.ErrNotFound):
		g.log.Info("grant: capability refused", append(fields, zap.String("reason", reasonNotLive))...)
		return v.refused(reasonNotLive)
	case err != nil:
		g.log.Warn("grant: charging the run grant failed", append(fields, zap.Error(err))...)
		return v.unavailable("identity store")
	}
	g.log.Info("grant: capability allowed", fields...)
	v.Allowed = true
	return v
}
