package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

func newRunGrantTestDeps(t *testing.T, slugs ...string) runGrantDeps {
	t.Helper()
	return runGrantDeps{
		identityDeps: newIdentityDeps(t, slugs...),
		ttlDefault:   10 * time.Minute, ttlMax: time.Hour,
		budgetDefault: 50, budgetMax: 1000,
	}
}

type runGrantOp func(context.Context, runGrantDeps, []byte) (event.Payload, error)

// callRunGrant runs one op as a rule of `stack` in `tenant`, in request rid.
func callRunGrant(t *testing.T, fn runGrantOp, d runGrantDeps, tenant, stack, metaJSON string) string {
	t.Helper()
	ctx := context.WithValue(context.Background(), config.CtxKeyRid, "rid-test")
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	if stack != "" {
		ctx = processor.WithStack(ctx, stack)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	// The envelope claims another stack and tenant: neither may be believed.
	pl, err := fn(ctx, d, []byte(`{"_txc":{"op":"evil/0/x","tenant":"evil","stack":"evil"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func TestRunGrantOps(t *testing.T) {
	d := newRunGrantTestDeps(t, "acme", "other")
	id := d.identityDeps
	grantee(t, id, "acme", "service:research")
	for _, g := range []string{
		`{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`,
		`{"principal":"service:research","kind":"capability","name":"crm.lookup"}`,
	} {
		if out := callIdentity(t, grantPut, id, "acme", "web", g); gjson.Get(out, "_grant.error").Exists() {
			t.Fatalf("grant: %s", out)
		}
	}

	// A rule on the stack's mail channel mints for a workspace of the stack.
	out := callRunGrant(t, runGrantMint, d, "acme", "web/_mail",
		`{"principal":"service:research","run":"task-1","allow":["secret:CRM_KEY","crm.lookup"],"workspace":"bench"}`)
	rg := gjson.Get(out, "_rungrant")
	gid := rg.Get("id").String()
	if !strings.HasPrefix(gid, "rgr_") || rg.Get("principal").String() != "service:research" ||
		rg.Get("run").String() != "task-1" || rg.Get("generation").Int() != 1 ||
		rg.Get("allow").Raw != `["crm.lookup","secret:CRM_KEY"]` ||
		rg.Get("budget.calls").Int() != 50 || rg.Get("spent.calls").Int() != 0 ||
		rg.Get("node_class").String() != "unreviewed" || rg.Get("minted_by").String() != "web" ||
		rg.Get("depth").Int() != 0 || !rg.Get("live").Bool() || rg.Get("parent").Exists() {
		t.Fatalf("mint = %s", out)
	}
	// The workspace is the calling stack's own, whatever channel the rule is on.
	if got, want := rg.Get("workspace_id").String(), workspace.ID("acme", "web", "bench"); got != want || rg.Get("workspace").String() != "bench" {
		t.Errorf("workspace id = %q, want %q", got, want)
	}
	issued, _ := time.Parse(time.RFC3339, rg.Get("issued_at").String())
	expires, _ := time.Parse(time.RFC3339, rg.Get("expires_at").String())
	if expires.Sub(issued) != 10*time.Minute {
		t.Errorf("default ttl: issued %s, expires %s", issued, expires)
	}
	// Nothing a rule should not hold is in the envelope.
	for _, leak := range []string{"token", "file_key", "rg1."} {
		if strings.Contains(out, leak) {
			t.Errorf("mint output holds %q: %s", leak, out)
		}
	}
	stored, err := id.store.ReadRunGrant(context.Background(), "tnt_acme", gid)
	if err != nil || stored.TraceID != "rid-test" || stored.Stack != "web" || stored.FileKey == "" || strings.Contains(out, stored.FileKey) {
		t.Errorf("stored grant: %+v err=%v", stored, err)
	}

	// Explicit limits, a reviewed node, and a slot's own workspace.
	out = callRunGrant(t, runGrantMint, d, "acme", "web/canary",
		`{"principal":"service:research","run":"task-2","allow":"crm.lookup","workspace":"bench","node_class":"reviewed","ttl":60,"budget":3,"generation":12,"into":"g"}`)
	if gjson.Get(out, "g.generation").Int() != 12 || gjson.Get(out, "g.budget.calls").Int() != 3 ||
		gjson.Get(out, "g.node_class").String() != "reviewed" ||
		gjson.Get(out, "g.workspace_id").String() != workspace.ID("acme", "web/canary", "bench") {
		t.Errorf("mint with limits = %s", out)
	}
	canaryID := gjson.Get(out, "g.id").String()

	for name, tc := range map[string]struct{ meta, code string }{
		"no principal":         {`{"run":"t","allow":["crm.lookup"]}`, "invalid_arg"},
		"no run":               {`{"principal":"service:research","allow":["crm.lookup"]}`, "invalid_arg"},
		"no allowlist":         {`{"principal":"service:research","run":"t"}`, "invalid_arg"},
		"a family":             {`{"principal":"service:research","run":"t","allow":["crm.*"]}`, "invalid_arg"},
		"a bad workspace":      {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"workspace":"../etc"}`, "invalid_arg"},
		"a bad node class":     {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"node_class":"trusted"}`, "invalid_arg"},
		"ttl past the ceiling": {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"ttl":3601}`, "invalid_arg"},
		"ttl of zero":          {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"ttl":0}`, "invalid_arg"},
		"ttl as a string":      {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"ttl":"60"}`, "invalid_arg"},
		"ttl with a fraction":  {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"ttl":1.5}`, "invalid_arg"},
		"budget past the cap":  {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"budget":1001}`, "invalid_arg"},
		"budget of zero":       {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"budget":0}`, "invalid_arg"},
		"generation of zero":   {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"generation":0}`, "invalid_arg"},
		"a token":              {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"token":"rg1.x.y"}`, "invalid_arg"},
		"a file key":           {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"file_key":"abc"}`, "invalid_arg"},
		"beyond its grants":    {`{"principal":"service:research","run":"t","allow":["secret:DB_DSN"]}`, "exceeds_standing"},
		"a stale generation":   {`{"principal":"service:research","run":"task-2","allow":["crm.lookup"],"generation":12}`, "stale_generation"},
		"a missing parent":     {`{"principal":"service:research","run":"t","allow":["crm.lookup"],"parent":"rgr_missing"}`, "parent_not_live"},
		"an unknown principal": {`{"principal":"service:ghost","run":"t","allow":["crm.lookup"]}`, "not_found"},
	} {
		wantIdentityCode(t, name, callRunGrant(t, runGrantMint, d, "acme", "web", tc.meta), "_rungrant", "txco_rungrant_"+tc.code)
	}
	out = callRunGrant(t, runGrantMint, d, "acme", "web", `{"principal":"service:research","run":"t","allow":["secret:DB_DSN"]}`)
	if msg := gjson.Get(out, "_rungrant.error.message").String(); !strings.Contains(msg, "secret:DB_DSN") || strings.Contains(msg, "authn:") {
		t.Errorf("exceeds_standing message should name the missing grant, in the author's terms: %q", msg)
	}
	out = callRunGrant(t, runGrantMint, d, "acme", "ingest", `{"principal":"service:research","run":"t","allow":["crm.lookup"]}`)
	wantIdentityCode(t, "mint from another stack", out, "_rungrant", "txco_rungrant_not_owner")

	// A grant narrowed from another.
	out = callRunGrant(t, runGrantMint, d, "acme", "web",
		`{"principal":"service:research","run":"task-1/lookup","allow":["crm.lookup"],"parent":"`+gid+`","budget":5}`)
	child := gjson.Get(out, "_rungrant.id").String()
	if child == "" || gjson.Get(out, "_rungrant.parent").String() != gid || gjson.Get(out, "_rungrant.depth").Int() != 1 {
		t.Fatalf("narrowed mint = %s", out)
	}
	wantIdentityCode(t, "more than the parent has left", callRunGrant(t, runGrantMint, d, "acme", "web",
		`{"principal":"service:research","run":"task-1/greedy","allow":["crm.lookup"],"parent":"`+gid+`","budget":46}`),
		"_rungrant", "txco_rungrant_exceeds_parent")

	// get: the minting stack only, never across tenants.
	out = callRunGrant(t, runGrantGet, d, "acme", "web/canary", `{"id":"`+gid+`"}`)
	if gjson.Get(out, "_rungrant.id").String() != gid || gjson.Get(out, "_rungrant.spent.calls").Int() != 5 || !gjson.Get(out, "_rungrant.live").Bool() {
		t.Errorf("get = %s", out)
	}
	wantIdentityCode(t, "get from another stack", callRunGrant(t, runGrantGet, d, "acme", "ingest", `{"id":"`+gid+`"}`), "_rungrant", "txco_rungrant_not_owner")
	wantIdentityCode(t, "get across tenants", callRunGrant(t, runGrantGet, d, "other", "web", `{"id":"`+gid+`"}`), "_rungrant", "txco_rungrant_not_found")
	wantIdentityCode(t, "get without id", callRunGrant(t, runGrantGet, d, "acme", "web", `{}`), "_rungrant", "txco_rungrant_invalid_arg")

	// close: the work ended.
	wantIdentityCode(t, "close from another stack", callRunGrant(t, runGrantClose, d, "acme", "ingest", `{"id":"`+canaryID+`"}`), "_rungrant", "txco_rungrant_not_owner")
	out = callRunGrant(t, runGrantClose, d, "acme", "web", `{"id":"`+canaryID+`","reason":"failed"}`)
	if !gjson.Get(out, "_rungrant.closed").Bool() || gjson.Get(out, "_rungrant.close_reason").String() != "failed" ||
		gjson.Get(out, "_rungrant.closed_at").String() == "" || gjson.Get(out, "_rungrant.live").Bool() {
		t.Errorf("close = %s", out)
	}
	if out := callRunGrant(t, runGrantClose, d, "acme", "web", `{"id":"`+canaryID+`"}`); gjson.Get(out, "_rungrant.closed").Bool() || gjson.Get(out, "_rungrant.error").Exists() {
		t.Errorf("second close = %s", out)
	}

	// revoke ends the grant and what was narrowed from it.
	wantIdentityCode(t, "revoke from another stack", callRunGrant(t, runGrantRevoke, d, "acme", "ingest", `{"id":"`+gid+`"}`), "_rungrant", "txco_rungrant_not_owner")
	out = callRunGrant(t, runGrantRevoke, d, "acme", "web", `{"id":"`+gid+`"}`)
	if !gjson.Get(out, "_rungrant.revoked").Bool() || gjson.Get(out, "_rungrant.revoked_at").String() == "" || gjson.Get(out, "_rungrant.live").Bool() {
		t.Errorf("revoke = %s", out)
	}
	if out := callRunGrant(t, runGrantRevoke, d, "acme", "web", `{"id":"`+gid+`"}`); gjson.Get(out, "_rungrant.revoked").Bool() || gjson.Get(out, "_rungrant.error").Exists() {
		t.Errorf("second revoke = %s", out)
	}
	if out := callRunGrant(t, runGrantGet, d, "acme", "web", `{"id":"`+child+`"}`); gjson.Get(out, "_rungrant.live").Bool() || gjson.Get(out, "_rungrant.revoked_at").String() == "" {
		t.Errorf("the narrowed grant outlived its parent: %s", out)
	}
	wantIdentityCode(t, "revoke a missing grant", callRunGrant(t, runGrantRevoke, d, "acme", "web", `{"id":"rgr_missing"}`), "_rungrant", "txco_rungrant_not_found")

	// No tenant, no stack, no store.
	mint := `{"principal":"service:research","run":"t","allow":["crm.lookup"]}`
	wantIdentityCode(t, "no tenant", callRunGrant(t, runGrantMint, d, "", "web", mint), "_rungrant", "txco_rungrant_no_tenant")
	wantIdentityCode(t, "no stack", callRunGrant(t, runGrantMint, d, "acme", "", mint), "_rungrant", "txco_rungrant_no_stack")
	off := d
	off.identityDeps = identityDeps{snap: id.snap}
	wantIdentityCode(t, "no store", callRunGrant(t, runGrantMint, off, "acme", "web", mint), "_rungrant", "txco_rungrant_disabled")
}
