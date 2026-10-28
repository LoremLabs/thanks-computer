package grantgw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/connection"
	"github.com/loremlabs/thanks-computer/chassis/grantwire"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\nBBBB\n-----END OPENSSH PRIVATE KEY-----\n"

// fileGrant: a sandbox with a variable, a file and a dotenv file.
func fileGrant(r *rig) authn.RunGrant {
	r.store("A", "a-value", secrets.PullAny)
	r.store("DEPLOY_KEY", key, secrets.PullAny)
	r.hold("A", "DEPLOY_KEY")
	return r.mint("task-1", func(n *authn.NewRunGrant) {
		n.BudgetCalls = 20
		n.Sandboxes = map[string]map[string]string{
			"box": {"A": "secret:A", "file:~/.ssh/id_ed25519": "secret:DEPLOY_KEY", "dotenv:run.env": "env"},
		}
	})
}

func (r *rig) openEntry(g authn.RunGrant, sandbox, entry string) Answer {
	r.t.Helper()
	return r.g.OpenSandbox(context.Background(), Open{Token: r.token(g), Sandbox: sandbox, Via: ViaNetwork, Entry: entry})
}

// TestAnOpenHandsOverOneEntry: starting a program hands over the variables
// only; a file's open hands over that file, byte for byte; the dotenv
// file's open the variables again. Each value is one request and one
// charge, and each answers the store's value at that moment.
func TestAnOpenHandsOverOneEntry(t *testing.T) {
	r := newRig(t)
	g := fileGrant(r)

	a := r.openEntry(g, "box", "")
	if !a.Allowed || len(a.Env) != 1 || string(a.Env["A"]) != "a-value" {
		t.Fatalf("start: %+v", a)
	}
	f := r.openEntry(g, "box", "file:~/.ssh/id_ed25519")
	if !f.Allowed || len(f.Env) != 1 || string(f.Env["file:~/.ssh/id_ed25519"]) != key {
		t.Fatalf("file: %+v", f)
	}
	last := r.seen[len(r.seen)-1]
	if gjson.Get(last, "_txc.grant.file").String() != "~/.ssh/id_ed25519" || gjson.Get(last, "_txc.grant.env").Exists() ||
		gjson.Get(last, "_txc.grant.via").String() != ViaNetwork || gjson.Get(last, "_txc.grant.name").String() != "DEPLOY_KEY" {
		t.Errorf("the file's request: %s", last)
	}
	d := r.openEntry(g, "box", "dotenv:run.env")
	if !d.Allowed || len(d.Env) != 1 || string(d.Env["A"]) != "a-value" {
		t.Fatalf("dotenv: %+v", d)
	}
	if r.runs != 3 || r.spent(g) != 3 {
		t.Errorf("%d runs, %d spent: one request and one charge per value", r.runs, r.spent(g))
	}

	// Changed in the store: the next open answers the new value.
	if _, err := r.secrets.RotateSecret(context.Background(), tenantID, nil, "A", []byte("a-rotated")); err != nil {
		t.Fatal(err)
	}
	if d = r.openEntry(g, "box", "dotenv:run.env"); string(d.Env["A"]) != "a-rotated" {
		t.Errorf("after rotation: %q", d.Env["A"])
	}

	// An entry the sandbox does not have: refused before any rule runs.
	before := r.runs
	for _, e := range []string{"file:other", "dotenv:other.env", "A", "file:/etc/passwd"} {
		wantRefused(t, e, r.openEntry(g, "box", e), reasonEntry)
	}
	if r.runs != before {
		t.Error("the stack ran for an entry the sandbox does not have")
	}
}

// TestARulesExecServesNoFile: the push form puts variables in one command's
// environment; a sandbox that serves a file is refused there, with where
// to serve it instead, and nothing is released.
func TestARulesExecServesNoFile(t *testing.T) {
	r := newRig(t)
	g := fileGrant(r)
	_, _, err := r.g.ForExec(context.Background(), tenantSlug, "web", "ws_bench", g.ID, []string{"box"})
	if err == nil || !strings.Contains(err.Error(), "txco sandbox serve box") {
		t.Fatalf("ForExec = %v", err)
	}
	if r.runs != 0 || r.spent(g) != 0 {
		t.Error("released before refusing")
	}
}

type fakeConns struct {
	mu      sync.Mutex
	token   string
	missing bool
	gotIn   connection.ReleaseInput
	calls   int
}

func (f *fakeConns) ReleaseCheck(_ context.Context, in connection.ReleaseInput) *connection.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotIn = in
	if f.missing {
		return &connection.Error{Code: connection.CodeMissing, Message: "connect it first"}
	}
	return nil
}

func (f *fakeConns) Release(_ context.Context, in connection.ReleaseInput) (string, connection.Row, *connection.Error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.token, connection.Row{Generation: 2}, nil
}

// TestAConnectionIsReleasedAsASecretIs: `connection:<provider>` hands over
// the principal's current token, decided in the same order (allowlist,
// standing, budget, the rules), with the provider and its status in the
// request; a principal with no connection is refused as not found.
func TestAConnectionIsReleasedAsASecretIs(t *testing.T) {
	r := newRig(t)
	conns := &fakeConns{token: "ghu_current"}
	r.g.conns = conns
	if _, _, err := r.ids.PutGrant(context.Background(), tenantID, "web", r.who,
		authn.NewGrant{Kind: authn.ResourceConnection, Name: "github"}); err != nil {
		t.Fatal(err)
	}
	g := r.mint("task-1", func(n *authn.NewRunGrant) {
		n.Sandboxes = map[string]map[string]string{"github": {"GH_TOKEN": "connection:github"}}
	})
	if !g.Allows(authn.Resource{Kind: authn.ResourceConnection, Name: "github"}) {
		t.Fatalf("the allowlist names the connection: %v", g.Allow)
	}

	a := r.openEntry(g, "github", "")
	if !a.Allowed || string(a.Env["GH_TOKEN"]) != "ghu_current" {
		t.Fatalf("release: %+v", a)
	}
	if conns.gotIn.Sandbox != "github" || conns.gotIn.Principal != r.who.ID || conns.gotIn.Stack != "web" || conns.gotIn.Tenant != tenantSlug {
		t.Errorf("released for %+v", conns.gotIn)
	}
	last := r.seen[len(r.seen)-1]
	if gjson.Get(last, "_txc.grant.kind").String() != "connection" || gjson.Get(last, "_txc.grant.connection.status").String() != "ok" ||
		gjson.Get(last, "_txc.grant.connection.provider").String() != "github" || strings.Contains(last, "ghu_current") {
		t.Errorf("the request: %s", last)
	}
	if r.spent(g) != 1 {
		t.Errorf("spent %d", r.spent(g))
	}

	// The token changes (renewed): the next open answers the new one.
	conns.token = "ghu_renewed"
	if a = r.openEntry(g, "github", ""); string(a.Env["GH_TOKEN"]) != "ghu_renewed" {
		t.Errorf("after renewal: %q", a.Env["GH_TOKEN"])
	}

	// Not connected: refused as not found, and the request says why.
	conns.missing = true
	r.g.seen = newRefusals(0)
	calls := conns.calls
	wantRefused(t, "missing", r.openEntry(g, "github", ""), reasonNotFound)
	if gjson.Get(r.seen[len(r.seen)-1], "_txc.grant.connection.status").String() != connection.CodeMissing || conns.calls != calls {
		t.Errorf("missing: status %s, released %d", gjson.Get(r.seen[len(r.seen)-1], "_txc.grant.connection.status"), conns.calls-calls)
	}
	conns.missing = false

	// The standing grant taken back: refused as standing.
	if _, _, err := r.ids.RevokeGrantOn(context.Background(), tenantID, "web", r.who, authn.ResourceConnection, "github"); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, "revoked", r.openEntry(g, "github", ""), reasonStanding)

	// No connections on this node: unavailable, not refused.
	r.g.conns = nil
	if a = r.openEntry(g, "github", ""); a.Allowed || !a.Unavailable {
		t.Errorf("no connections: %+v", a)
	}
}

// TestTheNetworkInlet: POST /v1/sandbox/{name} with the token opens one
// entry as the socket does, through grantwire.OpenURL; no token, a bad
// entry and a refusal each answer as the program should read them.
func TestTheNetworkInlet(t *testing.T) {
	r := newRig(t)
	g := fileGrant(r)
	router := mux.NewRouter()
	router.Path("/v1/sandbox/{name}").HandlerFunc(r.g.HandleSandbox).Methods(http.MethodPost)
	srv := httptest.NewServer(router)
	defer srv.Close()
	ctx := context.Background()

	got, err := grantwire.OpenURL(ctx, srv.Client(), srv.URL+"/", grantwire.Request{Token: r.token(g), Sandbox: "box", Entry: "file:~/.ssh/id_ed25519"})
	if err != nil || string(got.Env["file:~/.ssh/id_ed25519"]) != key {
		t.Fatalf("file over the network: %q %v", got, err)
	}
	if via := gjson.Get(r.seen[len(r.seen)-1], "_txc.grant.via").String(); via != ViaNetwork {
		t.Errorf("via = %q", via)
	}
	got, err = grantwire.OpenURL(ctx, srv.Client(), srv.URL, grantwire.Request{Token: r.token(g), Sandbox: "box"})
	if err != nil || string(got.Env["A"]) != "a-value" || len(got.Env) != 1 {
		t.Fatalf("variables: %q %v", got.Env, err)
	}
	if strings.Join(got.Serve, " ") != "dotenv:run.env file:~/.ssh/id_ed25519" {
		t.Errorf("serve = %v", got.Serve)
	}
	// The list: the files alone, nothing released.
	runs := r.runs
	got, err = grantwire.OpenURL(ctx, srv.Client(), srv.URL, grantwire.Request{Token: r.token(g), Sandbox: "box", Entry: grantwire.EntryList})
	if err != nil || len(got.Env) != 0 || len(got.Serve) != 2 || r.runs != runs {
		t.Errorf("list: %+v %v (%d runs)", got, err, r.runs-runs)
	}
	if _, err = grantwire.OpenURL(ctx, srv.Client(), srv.URL, grantwire.Request{Token: r.token(g), Sandbox: "other"}); !errors.Is(err, grantwire.ErrRefused) {
		t.Errorf("a sandbox the grant does not name: %v", err)
	}
	if _, err = grantwire.OpenURL(ctx, srv.Client(), srv.URL, grantwire.Request{Sandbox: "box"}); !errors.Is(err, grantwire.ErrRefused) {
		t.Errorf("no token: %v", err)
	}
	if _, err = grantwire.OpenURL(ctx, srv.Client(), srv.URL, grantwire.Request{Token: r.token(g), Sandbox: "box", Entry: "file:../x"}); !errors.Is(err, grantwire.ErrUnavailable) {
		t.Errorf("a bad entry is a bad request: %v", err)
	}
	if _, err = grantwire.OpenURL(ctx, srv.Client(), "ftp://x", grantwire.Request{Token: "t", Sandbox: "box"}); !errors.Is(err, grantwire.ErrUnavailable) {
		t.Errorf("not a URL: %v", err)
	}
}
