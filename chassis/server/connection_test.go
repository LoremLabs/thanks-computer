package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/connection"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/open"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

type testDecls struct{ p *connection.Provider }

func (d testDecls) Provider(_ context.Context, _, name string) (string, *connection.Provider, error) {
	if name != "github" {
		return "", nil, connection.ErrNotDeclared
	}
	return "tools-api", d.p, nil
}

func (d testDecls) Consumer(_ context.Context, _, _, _ string) (*connection.Consumer, error) {
	return nil, connection.ErrNotDeclared
}

type testSecrets map[string]string

func (s testSecrets) Materialize(_ context.Context, _, _, name string) ([]byte, error) {
	return []byte(s[name]), nil
}

func newConnectionDeps(t *testing.T) connectionDeps {
	t.Helper()
	d := newIdentityDeps(t, "acme")
	grantee(t, d, "acme", "service:pony-a")
	mk, err := secrets.NewInlineMasterKey(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))))
	if err != nil {
		t.Fatal(err)
	}
	p, err := connection.ParseProvider([]byte(`kind: oauth2
destinations:
  authorization: https://github.com/login/oauth/authorize
  token: https://github.com/login/oauth/access_token
  api: [https://api.github.com]
client_id: cid
client_secret: secret:GH_SECRET
consumers:
  tools-api: [api.github.read]
`))
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := connection.NewSigner([]byte(strings.Repeat("s", 32)), 1)
	node, _ := egress.Open("open", egress.Config{})
	svc, err := connection.New(connection.NewStore(d.store.DB, registry.SQLite, mk), testDecls{p}, testSecrets{"GH_SECRET": "x"}, signer, connection.Config{
		Node: node, Open: true, Logger: zap.NewNop(),
		SlugToID: func(_ context.Context, slug string) (string, error) { return "tnt_" + slug, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return connectionDeps{identityDeps: d, svc: svc}
}

type connectionOp func(context.Context, connectionDeps, []byte) (event.Payload, error)

func callConnection(t *testing.T, fn connectionOp, d connectionDeps, stack, metaJSON, envelope string) string {
	t.Helper()
	ctx := processor.WithStack(processor.WithTenant(context.Background(), "acme"), stack)
	ctx = operation.WithMeta(ctx, metaJSON)
	pl, err := fn(ctx, d, []byte(envelope))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

const httpEnvelope = `{"_txc":{"src":"http","web":{"req":{"host":"ranch.example","url":{"path":"/connect/github"}}}}}`

func TestConnectionOps(t *testing.T) {
	d := newConnectionDeps(t)
	meta := `{"principal":"service:pony-a","provider":"github","redirect_uri":"https://ranch.example/connect/github/callback"}`

	out := callConnection(t, connectionStart, d, "web", meta, httpEnvelope)
	u := gjson.Get(out, "_connection.url").String()
	if !strings.HasPrefix(u, "https://github.com/login/oauth/authorize?") || !strings.Contains(u, "state=") || gjson.Get(out, "_connection.principal").String() != "service:pony-a" {
		t.Fatalf("start = %s", out)
	}
	cookie := gjson.Get(out, "_txc.web.res.headers.set-cookie.0").String()
	if !strings.HasPrefix(cookie, connection.CookieName+"=") || !strings.Contains(cookie, "Secure") {
		t.Errorf("start must set the flow cookie on the response: %s", out)
	}
	// Only the managing stack; only on an HTTP request; only a known provider.
	wantIdentityCode(t, "start from another stack", callConnection(t, connectionStart, d, "ingest", meta, httpEnvelope), "_connection", "txco_connection_not_owner")
	wantIdentityCode(t, "start off http", callConnection(t, connectionStart, d, "web", meta, `{"_txc":{"src":"call"}}`), "_connection", "txco_connection_invalid_arg")
	wantIdentityCode(t, "start for an unknown provider", callConnection(t, connectionStart, d, "web", strings.Replace(meta, "github", "slack", 2), httpEnvelope), "_connection", "txco_connection_not_declared")
	wantIdentityCode(t, "start for an unwritten principal", callConnection(t, connectionStart, d, "web", strings.Replace(meta, "pony-a", "ghost", 1), httpEnvelope), "_connection", "txco_connection_not_found")

	// finish reads the request, not a WITH clause: a forged state is refused
	// before anything else is looked at.
	cb := `{"_txc":{"src":"http","web":{"req":{"host":"ranch.example","url":{"path":"/connect/github/callback","query":{"code":["c"],"state":["v1.forged.x"]}},"cookies":{"txco_connect":["v"]}}}}}`
	wantIdentityCode(t, "finish with a forged state", callConnection(t, connectionFinish, d, "web", `{}`, cb), "_connection", "txco_connection_bad_state")
	wantIdentityCode(t, "finish off http", callConnection(t, connectionFinish, d, "web", `{}`, `{"_txc":{"src":"cap"}}`), "_connection", "txco_connection_invalid_arg")

	// list and revoke: the managing stack; nothing yet.
	out = callConnection(t, connectionList, d, "web", `{"principal":"service:pony-a"}`, "{}")
	if gjson.Get(out, "_connection.count").Int() != 0 {
		t.Errorf("list = %s", out)
	}
	wantIdentityCode(t, "list from another stack", callConnection(t, connectionList, d, "ingest", `{"principal":"service:pony-a"}`, "{}"), "_connection", "txco_connection_not_owner")
	// By provider: the declaring stack and its consumers only.
	out = callConnection(t, connectionList, d, "tools-api", `{"provider":"github"}`, "{}")
	if gjson.Get(out, "_connection.provider").String() != "github" || gjson.Get(out, "_connection.count").Int() != 0 {
		t.Errorf("list by provider = %s", out)
	}
	wantIdentityCode(t, "list by provider from a stranger", callConnection(t, connectionList, d, "ingest", `{"provider":"github"}`, "{}"), "_connection", "txco_connection_not_owner")
	wantIdentityCode(t, "list both ways", callConnection(t, connectionList, d, "web", `{"principal":"service:pony-a","provider":"github"}`, "{}"), "_connection", "txco_connection_invalid_arg")
	out = callConnection(t, connectionRevoke, d, "web", `{"principal":"service:pony-a","provider":"github"}`, "{}")
	if gjson.Get(out, "_connection.revoked").Bool() {
		t.Errorf("revoke of nothing = %s", out)
	}
	wantIdentityCode(t, "revoke from another stack", callConnection(t, connectionRevoke, d, "ingest", `{"principal":"service:pony-a","provider":"github"}`, "{}"), "_connection", "txco_connection_not_owner")
	wantIdentityCode(t, "revoke without a provider", callConnection(t, connectionRevoke, d, "web", `{"principal":"service:pony-a"}`, "{}"), "_connection", "txco_connection_invalid_arg")

	// Without the service, every op says so.
	d.svc = nil
	wantIdentityCode(t, "disabled", callConnection(t, connectionStart, d, "web", meta, httpEnvelope), "_connection", "txco_connection_disabled")
}
