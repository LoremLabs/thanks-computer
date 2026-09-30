package grantgw

import (
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

const (
	srcName      = "grant"
	stackName    = "_grant"
	phaseRequest = "request"
)

// The reasons a request is refused. They are for the trace and the log: the
// program that asked is told only that it was refused.
const (
	reasonToken      = "token"      // no grant presented, or not one this chassis signed
	reasonExpired    = "expired"    // the token's own expiry passed
	reasonNotLive    = "not_live"   // the grant expired, ended or was revoked
	reasonGeneration = "generation" // the token is of an older generation of its run
	reasonTenant     = "tenant"     // the grant's tenant is gone
	reasonSandbox    = "sandbox"    // the run grant does not name the sandbox
	reasonName       = "name"       // not the name of a secret
	reasonKind       = "kind"       // a kind of resource nothing can hand over yet
	reasonNotFound   = "not_found"  // no such secret
	reasonAllowlist  = "allowlist"  // the run grant does not name it
	reasonStanding   = "standing"   // the principal holds no standing grant for it
	reasonPull       = "pull"       // the secret's pull policy does not admit this node
	reasonBudget     = "budget"     // the run grant's budget is spent
	reasonRule       = "rule"       // a _grant rule refused it
	reasonHeld       = "held"       // a _grant rule held it; a hold is a refusal in this build
	reasonVerdict    = "verdict"    // a _grant rule wrote a verdict that is not a boolean
	reasonError      = "error"      // the _grant run failed
	reasonTimeout    = "timeout"    // the _grant run did not answer in time
	reasonAdmission  = "admission"  // the tenant was refused admission
	reasonRoute      = "route"      // the request could not be routed to the tenant
)

// checks is the chassis's own reading of one request: the inputs of the
// proposal. exists is apart from the other four — a rule may overrule
// those, and cannot release what is not there.
type checks struct {
	exists    bool
	allowlist bool
	standing  bool
	pull      bool
	budget    bool
}

// proposal is the chassis's answer, made before any rule runs.
type proposal struct {
	allow  bool
	reason string // why not, when allow is false
}

// propose is the default action: allow when every check passes. The reason
// of a refusal is the first check that failed, outermost first.
func propose(c checks) proposal {
	switch {
	case !c.exists:
		return proposal{reason: reasonNotFound}
	case !c.allowlist:
		return proposal{reason: reasonAllowlist}
	case !c.standing:
		return proposal{reason: reasonStanding}
	case !c.pull:
		return proposal{reason: reasonPull}
	case !c.budget:
		return proposal{reason: reasonBudget}
	}
	return proposal{allow: true}
}

// facts is everything the chassis knows about one request.
type facts struct {
	rid    string
	tenant string // slug
	grant  authn.RunGrant
	res    authn.Resource
	rq     request
	meta   *secrets.SecretMetadata // nil: no such secret
	checks checks
	prop   proposal
	at     time.Time
}

// requestPayload builds the envelope the tenant's `_grant` stack is
// presented with. Everything in it is stamped here, from the run grant's own
// row and the stores — nothing the asking program sent except the name of
// the sandbox it opened, which the row was checked to name first. It never
// holds a secret's value, the grant's token or its file key.
func requestPayload(f facts) string {
	pb := jsonx.New()
	pb.Set("_txc.src", srcName)
	pb.Set("_txc.rid", f.rid)
	pb.Set("_txc.grant.phase", phaseRequest)
	pb.Set("_txc.grant.tenant", f.tenant)
	pb.Set("_txc.grant.kind", string(f.res.Kind))
	pb.Set("_txc.grant.name", f.res.Name)
	pb.Set("_txc.grant.verb", f.res.Verb())
	pb.Set("_txc.grant.via", f.rq.via)
	pb.Set("_txc.grant.sandbox", f.rq.sandbox)
	pb.Set("_txc.grant.env", f.rq.variable)

	pb.Set("_txc.grant.principal.id", f.grant.Principal.ID)
	pb.Set("_txc.grant.principal.kind", string(f.grant.Principal.Kind))
	pb.Set("_txc.grant.grant", f.grant.ID)
	pb.Set("_txc.grant.run", f.grant.Run)
	pb.Set("_txc.grant.generation", f.grant.Generation)
	pb.Set("_txc.grant.depth", f.grant.Depth)
	pb.Set("_txc.grant.stack", f.grant.Stack)
	if f.grant.Workspace != "" {
		pb.Set("_txc.grant.workspace", f.grant.Workspace)
	}
	pb.Set("_txc.grant.node.class", string(f.grant.NodeClass))

	if f.meta != nil {
		pull := f.meta.Pull
		if pull == "" {
			pull = secrets.PullNone
		}
		pb.Set("_txc.grant.secret.pull", string(pull))
		pb.Set("_txc.grant.secret.version", f.meta.VersionNo)
		scope := "tenant"
		if f.meta.Stack != nil {
			scope = "stack"
		}
		pb.Set("_txc.grant.secret.scope", scope)
	}

	pb.Set("_txc.grant.checks.exists", f.checks.exists)
	pb.Set("_txc.grant.checks.allowlist", f.checks.allowlist)
	pb.Set("_txc.grant.checks.standing", f.checks.standing)
	pb.Set("_txc.grant.checks.pull", f.checks.pull)
	pb.Set("_txc.grant.checks.budget", f.checks.budget)
	pb.Set("_txc.grant.budget.calls", f.grant.BudgetCalls)
	pb.Set("_txc.grant.budget.spent", f.grant.SpentCalls)

	pb.Set("_txc.grant.proposed.allow", f.prop.allow)
	if f.prop.reason != "" {
		pb.Set("_txc.grant.proposed.reason", f.prop.reason)
	}
	pb.Set("_ts", f.at.UTC().Format(time.RFC3339))
	return pb.String()
}

// verdict is what the gateway reads back from the `_grant` run.
type verdict struct {
	unavailable bool   // _txc.route.unavailable: the routing layer itself failed
	admission   bool   // _txc.admission.denied
	admReason   string // _txc.admission.reason

	set       bool   // a rule wrote a boolean at _txc.grant.res.allow
	allow     bool   // …and this is it
	malformed bool   // a rule wrote something there that is not a boolean
	hold      bool   // _txc.grant.res.hold
	reason    string // _txc.grant.res.reason, for the log
}

const maxRuleReason = 200

// parseVerdict interprets the final envelope of the `_grant` run. Pure.
func parseVerdict(raw string) verdict {
	f := gjson.GetMany(raw,
		"_txc.route.unavailable",
		"_txc.admission.denied", "_txc.admission.reason",
		"_txc.grant.res.allow", "_txc.grant.res.hold", "_txc.grant.res.reason")
	v := verdict{
		unavailable: f[0].Bool(),
		admission:   f[1].Bool(),
		admReason:   f[2].String(),
	}
	switch allow := f[3]; {
	case !allow.Exists() || allow.Type == gjson.Null:
	case allow.Type == gjson.True, allow.Type == gjson.False:
		v.set, v.allow = true, allow.Type == gjson.True
	default:
		// "true", 1, {"allow": true}: a rule that meant to decide and did
		// not. Reading any of these as a yes would turn a typo into a
		// release.
		v.malformed = true
	}
	// A hold is held only by the boolean true. Anything else there that is
	// not false or absent is a rule that meant to hold.
	switch hold := f[4]; {
	case !hold.Exists(), hold.Type == gjson.Null, hold.Type == gjson.False:
	case hold.Type == gjson.True:
		v.hold = true
	default:
		v.malformed = true
	}
	if r := f[5]; r.Type == gjson.String {
		v.reason = r.String()
		if len(v.reason) > maxRuleReason {
			v.reason = v.reason[:maxRuleReason]
		}
	}
	return v
}

// decision is the outcome of one request, before anything is charged or
// handed over.
type decision struct {
	allow  bool
	reason string // why not; "" when allowed
	// byRule: a rule's verdict decided it, not the proposal.
	byRule bool
	// note is the rule's own words, when it gave any.
	note string
}

// decide turns the proposal and the `_grant` run's outcome into the
// decision. runErr is the run's own failure; timedOut says it was the
// deadline.
//
// The order is the contract. A run that failed, was refused admission or
// could not be routed decides nothing, so the request is refused whatever
// the proposal was. Then a hold, then a malformed verdict, then the rule's
// verdict, then the proposal.
func decide(p proposal, v verdict, runErr error, timedOut bool) decision {
	switch {
	case timedOut:
		return decision{reason: reasonTimeout}
	case runErr != nil:
		return decision{reason: reasonError}
	case v.unavailable:
		return decision{reason: reasonRoute}
	case v.admission:
		return decision{reason: reasonAdmission}
	case v.hold:
		return decision{reason: reasonHeld, byRule: true, note: v.reason}
	case v.malformed:
		return decision{reason: reasonVerdict, byRule: true, note: v.reason}
	case v.set && v.allow:
		return decision{allow: true, byRule: true, note: v.reason}
	case v.set:
		return decision{reason: reasonRule, byRule: true, note: v.reason}
	case p.allow:
		return decision{allow: true}
	}
	return decision{reason: p.reason}
}
