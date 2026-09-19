package server

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
)

// wantBound asserts username signs in as principal in the tenant (by id).
func wantBound(t *testing.T, ids *authn.Store, tenantID, username, principal string) {
	t.Helper()
	b, err := ids.LookupBinding(context.Background(), tenantID, authn.BindEmail, "", username)
	if err != nil || b.Principal.ID != principal {
		t.Errorf("%s is bound to %q (err %v), want %s", username, b.Principal.ID, err, principal)
	}
}

// accountOpRefusals is what every account op refuses, now that an account
// holds no password and its username names a principal. username must be
// an account already bound to pony:paris by stack `web`; extra is spliced
// into every call's WITH (drive/account needs its collection).
func accountOpRefusals(t *testing.T, family, username, extra string, call func(stack, meta string) string) {
	t.Helper()
	code := func(out string) string { return gjson.Get(out, "_"+family+".error.code").String() }
	for _, param := range []string{`"password":"chosen-by-owner"`, `"password":""`, `"rotate":true`, `"password_style":"words"`, `"password_words":6`} {
		if got := code(call("web", `{"username":"`+username+`",`+param+extra+`}`)); got != "txco_"+family+"_invalid_arg" {
			t.Errorf("%s: %q, want txco_%s_invalid_arg", param, got, family)
		}
	}
	for meta, want := range map[string]string{
		// A new username must say who it signs in as.
		`{"username":"new@pony.example.com"` + extra + `}`:                                 "invalid_arg",
		`{"username":"new@pony.example.com","principal":"paris"` + extra + `}`:             "invalid_arg",
		`{"username":"new@pony.example.com","principal":"user:usr_22222222"` + extra + `}`: "invalid_arg",
		// A bound username keeps its principal.
		`{"username":"` + username + `","principal":"pony:milan"` + extra + `}`: "username_bound",
	} {
		if got := code(call("web", meta)); got != "txco_"+family+"_"+want {
			t.Errorf("%s → %q, want txco_%s_%s", meta, got, family, want)
		}
	}
	// Only the stack that manages the principal may change its account —
	// its canary slot counts as it. (The caller has disabled the account; a
	// refused "active" must leave it so, and the slot's "disabled" is a
	// no-op the caller can check.)
	if got := code(call("core", `{"username":"`+username+`","status":"active"`+extra+`}`)); got != "txco_"+family+"_not_owner" {
		t.Errorf("another stack: %q, want txco_%s_not_owner", got, family)
	}
	if got := code(call("web/canary", `{"username":"`+username+`","status":"disabled"`+extra+`}`)); got != "" {
		t.Errorf("the canary slot: %q, want no error", got)
	}
	if got := code(call("", `{"username":"`+username+`","status":"active"`+extra+`}`)); got != "txco_"+family+"_no_stack" {
		t.Errorf("no stack: %q, want txco_%s_no_stack", got, family)
	}
}
