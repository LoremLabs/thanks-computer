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
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// fakeSandboxes is a sandboxDecls over a map: "tenant/stack" → name → decl.
type fakeSandboxes map[string]map[string]*sandbox.Decl

func (f fakeSandboxes) Lookup(_ context.Context, tenant, stack, name string) (*sandbox.Decl, error) {
	if d, ok := f[tenant+"/"+stack][name]; ok {
		return d, nil
	}
	return nil, sandbox.ErrNotDeclared
}

func (f fakeSandboxes) Names(_ context.Context, tenant, stack string) ([]string, error) {
	return sandbox.DeclNames(f[tenant+"/"+stack]), nil
}

func decl(env map[string]string) *sandbox.Decl { return &sandbox.Decl{Env: env} }

func newRunGrantTestDeps(t *testing.T, slugs ...string) runGrantDeps {
	t.Helper()
	return runGrantDeps{
		identityDeps: newIdentityDeps(t, slugs...),
		sandboxes: fakeSandboxes{
			"acme/web": {
				"crm":  decl(map[string]string{"CRM_KEY": "secret:CRM_KEY"}),
				"db":   decl(map[string]string{"PGDSN": "secret:DB_DSN"}),
				"both": decl(map[string]string{"A": "secret:CRM_KEY", "B": "secret:DB_DSN"}),
			},
			"acme/web/canary": {"crm": decl(map[string]string{"CRM_KEY": "secret:CRM_KEY"})},
			"acme/ingest":     {"crm": decl(map[string]string{"CRM_KEY": "secret:CRM_KEY"})},
		},
		ttlDefault: 10 * time.Minute, ttlMax: time.Hour,
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

func TestDelegateOps(t *testing.T) {
	d := newRunGrantTestDeps(t, "acme", "other")
	id := d.identityDeps
	grantee(t, id, "acme", "service:research")
	if out := callIdentity(t, grantPut, id, "acme", "web", `{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`); gjson.Get(out, "_grant.error").Exists() {
		t.Fatalf("grant: %s", out)
	}

	// A rule on the stack's mail channel mints for a workspace of the stack.
	// `allow` names sandboxes of the app stack; the grant records what each
	// sets (the variables, never a value) and every secret they name.
	out := callRunGrant(t, delegateMint, d, "acme", "web/_mail",
		`{"principal":"service:research","run":"task-1","allow":["crm"," crm "],"workspace":"bench"}`)
	rg := gjson.Get(out, "_delegate")
	gid := rg.Get("id").String()
	if !strings.HasPrefix(gid, "rgr_") || rg.Get("principal").String() != "service:research" ||
		rg.Get("run").String() != "task-1" || rg.Get("generation").Int() != 1 ||
		rg.Get("allow").Raw != `["secret:CRM_KEY"]` || rg.Get("sandboxes").Raw != `{"crm":["CRM_KEY"]}` ||
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
	if err != nil || stored.TraceID != "rid-test" || stored.Stack != "web" || stored.FileKey == "" || strings.Contains(out, stored.FileKey) ||
		stored.Sandboxes["crm"]["CRM_KEY"] != "secret:CRM_KEY" {
		t.Errorf("stored grant: %+v err=%v", stored, err)
	}

	// Explicit limits, a reviewed node, and a slot's own workspace and
	// sandboxes (a canary declares its own, as it does its outlets).
	out = callRunGrant(t, delegateMint, d, "acme", "web/canary",
		`{"principal":"service:research","run":"task-2","allow":"crm","workspace":"bench","node_class":"reviewed","ttl":60,"budget":3,"generation":12,"into":"g"}`)
	if gjson.Get(out, "g.generation").Int() != 12 || gjson.Get(out, "g.budget.calls").Int() != 3 ||
		gjson.Get(out, "g.node_class").String() != "reviewed" ||
		gjson.Get(out, "g.workspace_id").String() != workspace.ID("acme", "web/canary", "bench") {
		t.Errorf("mint with limits = %s", out)
	}
	canaryID := gjson.Get(out, "g.id").String()

	for name, tc := range map[string]struct{ meta, code string }{
		"no principal":          {`{"run":"t","allow":["crm"]}`, "invalid_arg"},
		"no run":                {`{"principal":"service:research","allow":["crm"]}`, "invalid_arg"},
		"no allowlist":          {`{"principal":"service:research","run":"t"}`, "invalid_arg"},
		"an empty allowlist":    {`{"principal":"service:research","run":"t","allow":[]}`, "invalid_arg"},
		"a secret by name":      {`{"principal":"service:research","run":"t","allow":["secret:CRM_KEY"]}`, "invalid_arg"},
		"a capability":          {`{"principal":"service:research","run":"t","allow":["crm.lookup"]}`, "invalid_arg"},
		"a bad workspace":       {`{"principal":"service:research","run":"t","allow":["crm"],"workspace":"../etc"}`, "invalid_arg"},
		"a bad node class":      {`{"principal":"service:research","run":"t","allow":["crm"],"node_class":"trusted"}`, "invalid_arg"},
		"ttl past the ceiling":  {`{"principal":"service:research","run":"t","allow":["crm"],"ttl":3601}`, "invalid_arg"},
		"ttl of zero":           {`{"principal":"service:research","run":"t","allow":["crm"],"ttl":0}`, "invalid_arg"},
		"ttl as a string":       {`{"principal":"service:research","run":"t","allow":["crm"],"ttl":"60"}`, "invalid_arg"},
		"ttl with a fraction":   {`{"principal":"service:research","run":"t","allow":["crm"],"ttl":1.5}`, "invalid_arg"},
		"budget past the cap":   {`{"principal":"service:research","run":"t","allow":["crm"],"budget":1001}`, "invalid_arg"},
		"budget of zero":        {`{"principal":"service:research","run":"t","allow":["crm"],"budget":0}`, "invalid_arg"},
		"generation of zero":    {`{"principal":"service:research","run":"t","allow":["crm"],"generation":0}`, "invalid_arg"},
		"a token":               {`{"principal":"service:research","run":"t","allow":["crm"],"token":"rg1.x.y"}`, "invalid_arg"},
		"a file key":            {`{"principal":"service:research","run":"t","allow":["crm"],"file_key":"abc"}`, "invalid_arg"},
		"an undeclared sandbox": {`{"principal":"service:research","run":"t","allow":["crm","deploy"]}`, "no_sandbox"},
		"beyond its grants":     {`{"principal":"service:research","run":"t","allow":["db"]}`, "exceeds_standing"},
		"partly beyond them":    {`{"principal":"service:research","run":"t","allow":["both"]}`, "exceeds_standing"},
		"a stale generation":    {`{"principal":"service:research","run":"task-2","allow":["crm"],"generation":12}`, "stale_generation"},
		"a missing parent":      {`{"principal":"service:research","run":"t","allow":["crm"],"parent":"rgr_missing"}`, "parent_not_live"},
		"an unknown principal":  {`{"principal":"service:ghost","run":"t","allow":["crm"]}`, "not_found"},
	} {
		wantIdentityCode(t, name, callRunGrant(t, delegateMint, d, "acme", "web", tc.meta), "_delegate", "txco_delegate_"+tc.code)
	}
	out = callRunGrant(t, delegateMint, d, "acme", "web", `{"principal":"service:research","run":"t","allow":["both"]}`)
	if msg := gjson.Get(out, "_delegate.error.message").String(); !strings.Contains(msg, "secret:DB_DSN") || strings.Contains(msg, "CRM_KEY") || strings.Contains(msg, "authn:") {
		t.Errorf("exceeds_standing message should name the missing grant, in the author's terms: %q", msg)
	}
	out = callRunGrant(t, delegateMint, d, "acme", "web", `{"principal":"service:research","run":"t","allow":["deploy"]}`)
	if msg := gjson.Get(out, "_delegate.error.message").String(); !strings.Contains(msg, `"deploy"`) || !strings.Contains(msg, "both, crm, db") {
		t.Errorf("no_sandbox message should name the sandbox and what the stack has: %q", msg)
	}
	out = callRunGrant(t, delegateMint, d, "acme", "ingest", `{"principal":"service:research","run":"t","allow":["crm"]}`)
	wantIdentityCode(t, "mint from another stack", out, "_delegate", "txco_delegate_not_owner")
	// A stack with no declarations at all is told so.
	bare := d
	bare.sandboxes = fakeSandboxes{}
	out = callRunGrant(t, delegateMint, bare, "acme", "web", `{"principal":"service:research","run":"t","allow":["crm"]}`)
	wantIdentityCode(t, "no declarations", out, "_delegate", "txco_delegate_no_sandbox")
	if msg := gjson.Get(out, "_delegate.error.message").String(); !strings.Contains(msg, "SANDBOXES/crm.yaml") {
		t.Errorf("no_sandbox message should say where the file goes: %q", msg)
	}

	// A grant narrowed from another.
	out = callRunGrant(t, delegateMint, d, "acme", "web",
		`{"principal":"service:research","run":"task-1/lookup","allow":["crm"],"parent":"`+gid+`","budget":5}`)
	child := gjson.Get(out, "_delegate.id").String()
	if child == "" || gjson.Get(out, "_delegate.parent").String() != gid || gjson.Get(out, "_delegate.depth").Int() != 1 {
		t.Fatalf("narrowed mint = %s", out)
	}
	wantIdentityCode(t, "more than the parent has left", callRunGrant(t, delegateMint, d, "acme", "web",
		`{"principal":"service:research","run":"task-1/greedy","allow":["crm"],"parent":"`+gid+`","budget":46}`),
		"_delegate", "txco_delegate_exceeds_parent")

	// get: the minting stack only, never across tenants.
	out = callRunGrant(t, delegateGet, d, "acme", "web/canary", `{"id":"`+gid+`"}`)
	if gjson.Get(out, "_delegate.id").String() != gid || gjson.Get(out, "_delegate.spent.calls").Int() != 5 || !gjson.Get(out, "_delegate.live").Bool() {
		t.Errorf("get = %s", out)
	}
	wantIdentityCode(t, "get from another stack", callRunGrant(t, delegateGet, d, "acme", "ingest", `{"id":"`+gid+`"}`), "_delegate", "txco_delegate_not_owner")
	wantIdentityCode(t, "get across tenants", callRunGrant(t, delegateGet, d, "other", "web", `{"id":"`+gid+`"}`), "_delegate", "txco_delegate_not_found")
	wantIdentityCode(t, "get without id", callRunGrant(t, delegateGet, d, "acme", "web", `{}`), "_delegate", "txco_delegate_invalid_arg")

	// close: the work ended.
	wantIdentityCode(t, "close from another stack", callRunGrant(t, delegateClose, d, "acme", "ingest", `{"id":"`+canaryID+`"}`), "_delegate", "txco_delegate_not_owner")
	out = callRunGrant(t, delegateClose, d, "acme", "web", `{"id":"`+canaryID+`","reason":"failed"}`)
	if !gjson.Get(out, "_delegate.closed").Bool() || gjson.Get(out, "_delegate.close_reason").String() != "failed" ||
		gjson.Get(out, "_delegate.closed_at").String() == "" || gjson.Get(out, "_delegate.live").Bool() {
		t.Errorf("close = %s", out)
	}
	if out := callRunGrant(t, delegateClose, d, "acme", "web", `{"id":"`+canaryID+`"}`); gjson.Get(out, "_delegate.closed").Bool() || gjson.Get(out, "_delegate.error").Exists() {
		t.Errorf("second close = %s", out)
	}

	// revoke ends the grant and what was narrowed from it.
	wantIdentityCode(t, "revoke from another stack", callRunGrant(t, delegateRevoke, d, "acme", "ingest", `{"id":"`+gid+`"}`), "_delegate", "txco_delegate_not_owner")
	out = callRunGrant(t, delegateRevoke, d, "acme", "web", `{"id":"`+gid+`"}`)
	if !gjson.Get(out, "_delegate.revoked").Bool() || gjson.Get(out, "_delegate.revoked_at").String() == "" || gjson.Get(out, "_delegate.live").Bool() {
		t.Errorf("revoke = %s", out)
	}
	if out := callRunGrant(t, delegateRevoke, d, "acme", "web", `{"id":"`+gid+`"}`); gjson.Get(out, "_delegate.revoked").Bool() || gjson.Get(out, "_delegate.error").Exists() {
		t.Errorf("second revoke = %s", out)
	}
	if out := callRunGrant(t, delegateGet, d, "acme", "web", `{"id":"`+child+`"}`); gjson.Get(out, "_delegate.live").Bool() || gjson.Get(out, "_delegate.revoked_at").String() == "" {
		t.Errorf("the narrowed grant outlived its parent: %s", out)
	}
	wantIdentityCode(t, "revoke a missing grant", callRunGrant(t, delegateRevoke, d, "acme", "web", `{"id":"rgr_missing"}`), "_delegate", "txco_delegate_not_found")

	// No tenant, no stack, no store.
	mint := `{"principal":"service:research","run":"t","allow":["crm"]}`
	wantIdentityCode(t, "no tenant", callRunGrant(t, delegateMint, d, "", "web", mint), "_delegate", "txco_delegate_no_tenant")
	wantIdentityCode(t, "no stack", callRunGrant(t, delegateMint, d, "acme", "", mint), "_delegate", "txco_delegate_no_stack")
	off := d
	off.identityDeps = identityDeps{snap: id.snap}
	wantIdentityCode(t, "no store", callRunGrant(t, delegateMint, off, "acme", "web", mint), "_delegate", "txco_delegate_disabled")
}
