package grantgw

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
)

// holdCap gives the principal a standing grant to invoke each capability.
func (r *rig) holdCap(names ...string) {
	r.t.Helper()
	for _, n := range names {
		if _, _, err := r.ids.PutGrant(context.Background(), tenantID, "web", r.who,
			authn.NewGrant{Kind: authn.ResourceCapability, Name: n}); err != nil {
			r.t.Fatal(err)
		}
	}
}

// mintCaps writes a run grant whose one sandbox names capabilities and no
// variable, as txco://delegate/mint does from a `capabilities:` declaration.
func (r *rig) mintCaps(run string, names ...string) authn.RunGrant {
	r.t.Helper()
	return r.mint(run, func(in *authn.NewRunGrant) {
		in.Allow = names
		in.Sandboxes = map[string]map[string]string{"workstation": {}}
	})
}

func (r *rig) call(g authn.RunGrant, name string, input string) Verdict {
	r.t.Helper()
	return r.g.Invoke(context.Background(), Call{Token: r.token(g), Name: name, Input: json.RawMessage(input)})
}

func wantAllowed(t *testing.T, what string, v Verdict) {
	t.Helper()
	if !v.Allowed || v.Held || v.Unavailable || v.Reason != "" {
		t.Errorf("%s: %+v, want allowed", what, v)
	}
}

func wantCapRefused(t *testing.T, what string, v Verdict, reason string) {
	t.Helper()
	if v.Allowed || v.Held || v.Unavailable || v.Reason != reason {
		t.Errorf("%s: allowed=%v held=%v unavailable=%v reason=%q, want refused for %q", what, v.Allowed, v.Held, v.Unavailable, v.Reason, reason)
	}
}

// The proposal stands for a capability as for a secret, and the call is
// presented to the rules with its input.
func TestInvokeCapabilityProposalStands(t *testing.T) {
	r := newRig(t)
	r.holdCap("card.note", "ai.chat")
	g := r.mintCaps("task-1", "card.note", "ai.chat")

	v := r.call(g, "card.note", `{"text":"hello"}`)
	wantAllowed(t, "card.note", v)
	if v.Tenant != tenantSlug || v.Grant.ID != g.ID || !v.Identified() {
		t.Errorf("verdict names %q / %q, want the tenant and the grant", v.Tenant, v.Grant.ID)
	}
	if got := r.spent(g); got != 1 {
		t.Errorf("spent = %d, want 1: an allowed call is charged", got)
	}
	if r.runs != 1 {
		t.Fatalf("the request was presented %d times, want 1", r.runs)
	}
	env := r.seen[0]
	for path, want := range map[string]any{
		"_txc.src": "grant", "_txc.grant.phase": "request", "_txc.grant.tenant": tenantSlug,
		"_txc.grant.kind": "capability", "_txc.grant.name": "card.note", "_txc.grant.verb": "invoke",
		"_txc.grant.via": "http", "_txc.grant.principal.id": "service:research",
		"_txc.grant.grant": g.ID, "_txc.grant.run": "task-1", "_txc.grant.input.text": "hello",
		"_txc.grant.checks.allowlist": "true", "_txc.grant.checks.standing": "true",
		"_txc.grant.checks.budget": "true", "_txc.grant.proposed.allow": "true",
	} {
		if got := gjson.Get(env, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	for _, absent := range []string{"_txc.grant.sandbox", "_txc.grant.env", "_txc.grant.secret"} {
		if gjson.Get(env, absent).Exists() {
			t.Errorf("%s is set on a capability request", absent)
		}
	}
	if gjson.Get(env, "@this").String() == "" || contains(env, r.token(g)) {
		t.Errorf("the token reached the envelope")
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// Every hard check and every proposal check refuses on its own.
func TestInvokeCapabilityRefusals(t *testing.T) {
	r := newRig(t)
	r.holdCap("card.note")
	g := r.mintCaps("task-1", "card.note")

	wantCapRefused(t, "not in the allowlist", r.call(g, "mail.send", `{}`), reasonAllowlist)
	// Named, but the principal holds no standing grant: the mint would have
	// refused it, so it is a row that changed under us.
	if _, err := r.ids.DB.Exec(r.ids.Dialect.Rebind(`UPDATE run_grants SET allowlist = ? WHERE id = ?`), `["card.note","ai.chat"]`, g.ID); err != nil {
		t.Fatal(err)
	}
	wantCapRefused(t, "no standing grant", r.call(g, "ai.chat", `{}`), reasonStanding)
	wantCapRefused(t, "not a name", r.call(g, "Card.Note", `{}`), reasonName)
	v := r.g.Invoke(context.Background(), Call{Token: "rg1.not.a.token", Name: "card.note"})
	wantCapRefused(t, "bad token", v, reasonToken)
	if v.Identified() {
		t.Errorf("a bad token identified a grant")
	}
	if got := r.spent(g); got != 0 {
		t.Errorf("refusals spent %d", got)
	}

	// The budget: one call per request, and then no more.
	g2 := r.mint("task-2", func(in *authn.NewRunGrant) {
		in.Allow, in.Sandboxes, in.BudgetCalls = []string{"card.note"}, map[string]map[string]string{"workstation": {}}, 2
	})
	wantAllowed(t, "first", r.call(g2, "card.note", `{}`))
	wantAllowed(t, "second", r.call(g2, "card.note", `{}`))
	wantCapRefused(t, "third", r.call(g2, "card.note", `{}`), reasonBudget)

	// Closed: refused before any rule.
	if _, _, err := r.ids.CloseRunGrant(context.Background(), tenantID, "web", g.ID, "done"); err != nil {
		t.Fatal(err)
	}
	runs := r.runs
	wantCapRefused(t, "closed", r.call(g, "card.note", `{}`), reasonNotLive)
	if r.runs != runs {
		t.Errorf("a closed grant ran the rules")
	}
}

// A rule may refuse, allow or hold. A hold is not remembered: the same call
// after the person decided is decided afresh.
func TestInvokeCapabilityRules(t *testing.T) {
	r := newRig(t)
	r.holdCap("mail.send")
	g := r.mintCaps("task-1", "mail.send")

	r.rules = rule("_txc.grant.res.allow", false, "_txc.grant.res.reason", "not today")
	wantCapRefused(t, "a rule refuses", r.call(g, "mail.send", `{"to":"x@example.com"}`), reasonRule)
	if got := r.spent(g); got != 0 {
		t.Errorf("a refusal spent %d", got)
	}

	r.rules = rule("_txc.grant.res.hold", true, "_txc.grant.res.reason", "an outside recipient")
	r.now = r.now.Add(10 * 1e9) // past the refusal window
	v := r.call(g, "mail.send", `{"to":"x@example.com"}`)
	if !v.Held || v.Allowed || v.Unavailable || v.Reason != reasonHeld || !v.Identified() {
		t.Errorf("a rule holds: %+v", v)
	}
	if got := r.spent(g); got != 0 {
		t.Errorf("a hold spent %d", got)
	}
	// Decided: the next call is presented again, not answered from memory.
	r.rules = rule("_txc.grant.res.allow", true)
	runs := r.runs
	wantAllowed(t, "after the hold", r.call(g, "mail.send", `{"to":"x@example.com"}`))
	if r.runs != runs+1 {
		t.Errorf("the call after a hold was not presented (runs %d → %d)", runs, r.runs)
	}
	if got := r.spent(g); got != 1 {
		t.Errorf("spent = %d, want 1", got)
	}
}
