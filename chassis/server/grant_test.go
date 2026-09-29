package server

import (
	"context"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
)

// grantee makes a product principal that exists and is managed by `web`.
func grantee(t *testing.T, d identityDeps, tenant, principal string) {
	t.Helper()
	out := callIdentity(t, credentialCreate, d, tenant, "web", `{"principal":"`+principal+`","scopes":["drive:*:*"]}`)
	if gjson.Get(out, "_credential.id").String() == "" {
		t.Fatalf("make %s: %s", principal, out)
	}
}

func TestGrantOps(t *testing.T) {
	d := newIdentityDeps(t, "acme", "other")
	grantee(t, d, "acme", "service:research")
	grantee(t, d, "acme", "service:billing")

	out := callIdentity(t, grantPut, d, "acme", "web", `{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`)
	id := gjson.Get(out, "_grant.id").String()
	if !strings.HasPrefix(id, "grt_") || gjson.Get(out, "_grant.principal").String() != "service:research" ||
		gjson.Get(out, "_grant.kind").String() != "secret" || gjson.Get(out, "_grant.name").String() != "CRM_KEY" ||
		gjson.Get(out, "_grant.verbs").Raw != `["release"]` || gjson.Get(out, "_grant.created_by").String() != "web" ||
		!gjson.Get(out, "_grant.created").Bool() || gjson.Get(out, "_grant.revoked_at").Exists() {
		t.Fatalf("put = %s", out)
	}
	// The row is keyed by the tenant's ID, resolved from the pinned slug.
	p, _ := authn.ParsePrincipal("service:research")
	if ok, err := d.store.Granted(context.Background(), "tnt_acme", p, authn.Resource{Kind: authn.ResourceSecret, Name: "CRM_KEY"}, authn.VerbRelease); err != nil || !ok {
		t.Errorf("stored grant: granted=%v err=%v", ok, err)
	}
	// Safe to retry, from the stack or its canary slot.
	out = callIdentity(t, grantPut, d, "acme", "web/canary", `{"principal":"service:research","kind":"secret","name":"CRM_KEY","verbs":"release","into":"g"}`)
	if gjson.Get(out, "g.id").String() != id || gjson.Get(out, "g.created").Bool() {
		t.Errorf("re-put = %s", out)
	}
	out = callIdentity(t, grantPut, d, "acme", "web", `{"principal":"service:research","kind":"capability","name":"crm.lookup"}`)
	capID := gjson.Get(out, "_grant.id").String()
	if capID == "" || gjson.Get(out, "_grant.verbs").Raw != `["invoke"]` {
		t.Fatalf("capability put = %s", out)
	}

	// Another stack is told who to ask; an unwritten principal is not found.
	out = callIdentity(t, grantPut, d, "acme", "ingest", `{"principal":"service:research","kind":"secret","name":"DB_DSN"}`)
	wantIdentityCode(t, "put from another stack", out, "_grant", "txco_grant_not_owner")
	if msg := gjson.Get(out, "_grant.error.message").String(); !strings.Contains(msg, `"web"`) || strings.Contains(msg, "authn:") {
		t.Errorf("not_owner message should name the owner, in the author's terms: %q", msg)
	}
	wantIdentityCode(t, "put to an unwritten principal",
		callIdentity(t, grantPut, d, "acme", "web", `{"principal":"service:ghost","kind":"secret","name":"CRM_KEY"}`), "_grant", "txco_grant_not_found")
	wantIdentityCode(t, "put in a tenant that does not know the principal",
		callIdentity(t, grantPut, d, "other", "web", `{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`), "_grant", "txco_grant_not_found")

	for name, meta := range map[string]string{
		"no principal":   `{"kind":"secret","name":"CRM_KEY"}`,
		"bad principal":  `{"principal":"research","kind":"secret","name":"CRM_KEY"}`,
		"no kind":        `{"principal":"service:research","name":"CRM_KEY"}`,
		"no name":        `{"principal":"service:research","kind":"secret"}`,
		"unknown kind":   `{"principal":"service:research","kind":"drive","name":"dc_1"}`,
		"bad name":       `{"principal":"service:research","kind":"secret","name":"crm.key"}`,
		"a family":       `{"principal":"service:research","kind":"capability","name":"crm.*"}`,
		"the wrong verb": `{"principal":"service:research","kind":"secret","name":"CRM_KEY","verbs":["invoke"]}`,
	} {
		wantIdentityCode(t, name, callIdentity(t, grantPut, d, "acme", "web", meta), "_grant", "txco_grant_invalid_arg")
	}

	// list: what a principal holds (the owner only)…
	out = callIdentity(t, grantList, d, "acme", "web", `{"principal":"service:research"}`)
	if gjson.Get(out, "_grant.count").Int() != 2 || gjson.Get(out, "_grant.principal").String() != "service:research" ||
		gjson.Get(out, "_grant.items.0.id").String() != capID || gjson.Get(out, "_grant.items.1.id").String() != id {
		t.Errorf("list = %s", out)
	}
	out = callIdentity(t, grantList, d, "acme", "web", `{"principal":"service:research","kind":"secret"}`)
	if gjson.Get(out, "_grant.count").Int() != 1 || gjson.Get(out, "_grant.items.0.name").String() != "CRM_KEY" {
		t.Errorf("list of one kind = %s", out)
	}
	wantIdentityCode(t, "list from another stack",
		callIdentity(t, grantList, d, "acme", "ingest", `{"principal":"service:research"}`), "_grant", "txco_grant_not_owner")
	// …or who holds a resource (any stack).
	callIdentity(t, grantPut, d, "acme", "web", `{"principal":"service:billing","kind":"secret","name":"CRM_KEY"}`)
	out = callIdentity(t, grantList, d, "acme", "ingest", `{"kind":"secret","name":"CRM_KEY"}`)
	if gjson.Get(out, "_grant.count").Int() != 2 || gjson.Get(out, "_grant.kind").String() != "secret" ||
		gjson.Get(out, "_grant.name").String() != "CRM_KEY" || gjson.Get(out, "_grant.items.0.principal").String() != "service:billing" {
		t.Errorf("list by resource = %s", out)
	}
	if out := callIdentity(t, grantList, d, "other", "web", `{"kind":"secret","name":"CRM_KEY"}`); gjson.Get(out, "_grant.count").Int() != 0 {
		t.Errorf("list by resource across tenants = %s", out)
	}
	wantIdentityCode(t, "list with nothing", callIdentity(t, grantList, d, "acme", "web", `{}`), "_grant", "txco_grant_invalid_arg")
	wantIdentityCode(t, "list with both", callIdentity(t, grantList, d, "acme", "web",
		`{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`), "_grant", "txco_grant_invalid_arg")

	// revoke: by id, pinned to a principal, or by what the grant names.
	wantIdentityCode(t, "revoke from another stack",
		callIdentity(t, grantRevoke, d, "acme", "ingest", `{"id":"`+id+`"}`), "_grant", "txco_grant_not_owner")
	wantIdentityCode(t, "revoke another principal's id",
		callIdentity(t, grantRevoke, d, "acme", "web", `{"id":"`+id+`","principal":"service:billing"}`), "_grant", "txco_grant_not_found")
	wantIdentityCode(t, "revoke across tenants",
		callIdentity(t, grantRevoke, d, "other", "web", `{"id":"`+id+`"}`), "_grant", "txco_grant_not_found")
	out = callIdentity(t, grantRevoke, d, "acme", "web", `{"id":"`+id+`","principal":"service:research"}`)
	if !gjson.Get(out, "_grant.revoked").Bool() || gjson.Get(out, "_grant.revoked_at").String() == "" || gjson.Get(out, "_grant.id").String() != id {
		t.Errorf("revoke = %s", out)
	}
	if out := callIdentity(t, grantRevoke, d, "acme", "web", `{"id":"`+id+`"}`); gjson.Get(out, "_grant.revoked").Bool() || gjson.Get(out, "_grant.error").Exists() {
		t.Errorf("second revoke = %s", out)
	}
	out = callIdentity(t, grantRevoke, d, "acme", "web", `{"principal":"service:research","kind":"capability","name":"crm.lookup"}`)
	if !gjson.Get(out, "_grant.revoked").Bool() || gjson.Get(out, "_grant.id").String() != capID {
		t.Errorf("revoke by name = %s", out)
	}
	wantIdentityCode(t, "revoke a grant never given", callIdentity(t, grantRevoke, d, "acme", "web",
		`{"principal":"service:research","kind":"capability","name":"mail.send"}`), "_grant", "txco_grant_not_found")
	for name, meta := range map[string]string{
		"nothing":            `{}`,
		"id and a name":      `{"id":"` + id + `","kind":"secret","name":"CRM_KEY"}`,
		"principal alone":    `{"principal":"service:research"}`,
		"a bad principal":    `{"principal":"research","kind":"secret","name":"CRM_KEY"}`,
		"kind without name":  `{"principal":"service:research","kind":"secret"}`,
		"name of a bad kind": `{"principal":"service:research","kind":"drive","name":"dc_1"}`,
	} {
		wantIdentityCode(t, name, callIdentity(t, grantRevoke, d, "acme", "web", meta), "_grant", "txco_grant_invalid_arg")
	}
	if out := callIdentity(t, grantList, d, "acme", "web", `{"principal":"service:research","include_revoked":true}`); gjson.Get(out, "_grant.count").Int() != 2 {
		t.Errorf("full list = %s", out)
	}

	// No tenant, no stack, no store.
	wantIdentityCode(t, "no tenant", callIdentity(t, grantPut, d, "", "web", `{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`), "_grant", "txco_grant_no_tenant")
	wantIdentityCode(t, "unknown tenant", callIdentity(t, grantPut, d, "nowhere", "web", `{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`), "_grant", "txco_grant_no_tenant")
	wantIdentityCode(t, "no stack", callIdentity(t, grantPut, d, "acme", "", `{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`), "_grant", "txco_grant_no_stack")
	wantIdentityCode(t, "no store", callIdentity(t, grantPut, identityDeps{snap: d.snap}, "acme", "web", `{"principal":"service:research","kind":"secret","name":"CRM_KEY"}`), "_grant", "txco_grant_disabled")
}
