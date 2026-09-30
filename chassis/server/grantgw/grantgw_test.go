package grantgw

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/rungrant"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	dbschemas "github.com/loremlabs/thanks-computer/db"
)

const (
	tenantID   = "tnt_acme"
	tenantSlug = "acme"
)

// rig is a gateway over real stores, with the tenant's `_grant` stack played
// by a function.
type rig struct {
	t       *testing.T
	g       *Gateway
	ids     *authn.Store
	secrets *secrets.Store
	signer  *rungrant.Signer
	who     authn.Principal
	now     time.Time

	mu    sync.Mutex
	runs  int      // times the stack ran
	seen  []string // the envelopes it was presented with
	pins  []string // the tenant/stack pinned on each run's context
	rules func(envelope string) (string, error)
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	r := &rig{t: t, now: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}

	r.ids = authntest.NewSQLiteStore(t)
	r.ids.SetClock(func() time.Time { return r.now })

	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := dbschemas.ApplyRuntimeSQLite(db); err != nil {
		t.Fatalf("runtime schema: %v", err)
	}
	mk, err := secrets.NewInlineMasterKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	r.secrets = secrets.NewStore(db, mk)

	key, ver, err := secrets.DeriveKey(mk, rungrant.KeyLabel)
	if err != nil {
		t.Fatal(err)
	}
	if r.signer, err = rungrant.New(key, ver); err != nil {
		t.Fatal(err)
	}

	r.who, _ = authn.ParsePrincipal("service:research")
	if _, _, err := r.ids.IssueCredential(ctx, tenantID, "web", r.who, authn.NewCredential{Scopes: []string{"drive:*:*"}}); err != nil {
		t.Fatal(err)
	}

	r.g = &Gateway{
		ctx: ctx, log: zap.NewNop(), ids: r.ids, signer: r.signer,
		timeout: time.Second, seen: newRefusals(5 * time.Second),
		now: func() time.Time { return r.now },
		tenantSlug: func(_ context.Context, id string) (string, error) {
			if id == tenantID {
				return tenantSlug, nil
			}
			return "", errNoTenant
		},
		tenantID: func(_ context.Context, slug string) (string, error) {
			if slug == tenantSlug {
				return tenantID, nil
			}
			return "", errNoTenant
		},
		resolve: r.secrets.ResolveForRelease,
		decrypt: r.secrets.DecryptResolved,
		runStack: func(ctx context.Context, payload string) (string, error) {
			r.mu.Lock()
			r.runs++
			r.seen = append(r.seen, payload)
			r.pins = append(r.pins, processor.TenantScope(ctx)+"/"+processor.StackScope(ctx))
			rules := r.rules
			r.mu.Unlock()
			if rules == nil {
				return payload, nil // a tenant with no _grant rules
			}
			return rules(payload)
		},
		bin: "/usr/local/bin/txco",
	}
	return r
}

// store keeps a secret with a pull policy, tenant-wide.
func (r *rig) store(name, value string, pull secrets.PullPolicy) {
	r.t.Helper()
	ctx := context.Background()
	if _, err := r.secrets.CreateSecret(ctx, tenantID, nil, name, "", "op", []byte(value)); err != nil {
		r.t.Fatal(err)
	}
	if pull != secrets.PullNone {
		if _, err := r.secrets.UpdateSecretPull(ctx, tenantID, nil, name, pull); err != nil {
			r.t.Fatal(err)
		}
	}
}

// hold gives the principal a standing grant to release each secret.
func (r *rig) hold(names ...string) {
	r.t.Helper()
	for _, n := range names {
		if _, _, err := r.ids.PutGrant(context.Background(), tenantID, "web", r.who,
			authn.NewGrant{Kind: authn.ResourceSecret, Name: n}); err != nil {
			r.t.Fatal(err)
		}
	}
}

// box is the sandbox a test's secret is opened through: one per secret,
// named for it, setting one variable of the secret's own name.
func box(name string) string { return strings.ToLower(strings.ReplaceAll(name, "_", "-")) }

// mint writes a run grant that names one sandbox per secret.
func (r *rig) mint(run string, mut func(*authn.NewRunGrant), names ...string) authn.RunGrant {
	r.t.Helper()
	in := authn.NewRunGrant{Run: run, Stack: "web", Workspace: "bench", WorkspaceID: "ws_bench",
		BudgetCalls: 5, TTL: 10 * time.Minute, Sandboxes: map[string]map[string]string{}}
	for _, n := range names {
		in.Sandboxes[box(n)] = map[string]string{n: "secret:" + n}
	}
	if mut != nil {
		mut(&in)
	}
	g, err := r.ids.MintRunGrant(context.Background(), tenantID, "web", r.who, in)
	if err != nil {
		r.t.Fatalf("mint %s: %v", run, err)
	}
	return g
}

// ready is the common case: a stored secret, a standing grant, a run grant.
func (r *rig) ready(pull secrets.PullPolicy) authn.RunGrant {
	r.t.Helper()
	r.store("DB_DSN", "postgres://secret-value", pull)
	r.hold("DB_DSN")
	return r.mint("task-1", nil, "DB_DSN")
}

// ask opens the secret's sandbox the way the exec does: by the row.
func (r *rig) ask(g authn.RunGrant, name string) Answer {
	r.t.Helper()
	return r.g.OpenSandbox(context.Background(), Open{TenantID: tenantID, GrantID: g.ID, Sandbox: box(name), Via: ViaExec})
}

// open opens a sandbox the way the launcher does: by the token.
func (r *rig) open(g authn.RunGrant, sandbox string) Answer {
	r.t.Helper()
	return r.g.OpenSandbox(context.Background(), Open{Token: r.token(g), Sandbox: sandbox, Via: ViaLauncher})
}

func (r *rig) token(g authn.RunGrant) string {
	r.t.Helper()
	tok, err := r.signer.Sign(rungrant.Claims{Grant: g.ID, Tenant: g.TenantID, Principal: g.Principal.ID,
		Run: g.Run, Generation: g.Generation, Expires: g.ExpiresAt.Unix()})
	if err != nil {
		r.t.Fatal(err)
	}
	return tok
}

func (r *rig) spent(g authn.RunGrant) int64 {
	r.t.Helper()
	got, err := r.ids.ReadRunGrant(context.Background(), tenantID, g.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return got.SpentCalls
}

// rule is a _grant rule: it sets paths on the envelope it is presented with.
func rule(sets ...any) func(string) (string, error) {
	return func(env string) (string, error) {
		for i := 0; i+1 < len(sets); i += 2 {
			env, _ = sjson.Set(env, sets[i].(string), sets[i+1])
		}
		return env, nil
	}
}

func wantRefused(t *testing.T, what string, a Answer, reason string) {
	t.Helper()
	if a.Allowed || a.Unavailable || a.Reason != reason || len(a.Env) != 0 {
		t.Errorf("%s: %+v, want refused for %q", what, a, reason)
	}
}

// wantValue: the sandbox opened with exactly one variable holding value.
func wantValue(t *testing.T, what string, a Answer, value string) {
	t.Helper()
	var got string
	for _, v := range a.Env {
		got = string(v)
	}
	if !a.Allowed || a.Unavailable || len(a.Env) != 1 || got != value {
		t.Errorf("%s: allowed=%v unavailable=%v reason=%q env=%q, want %q", what, a.Allowed, a.Unavailable, a.Reason, a.Env, value)
	}
}

// With no _grant rules the chassis's proposal is the answer.
func TestTheProposalStandsWhenNoRuleDecides(t *testing.T) {
	r := newRig(t)
	g := r.ready(secrets.PullNone)

	// The default policy refuses a pull: the secret is used where it is stored.
	wantRefused(t, "pull none", r.ask(g, "DB_DSN"), reasonPull)
	if r.runs != 1 {
		t.Errorf("the request was presented %d times, want 1: a refusal is traced too", r.runs)
	}
	if got := r.spent(g); got != 0 {
		t.Errorf("a refusal spent %d calls", got)
	}
	env := r.seen[0]
	for path, want := range map[string]any{
		"_txc.src": "grant", "_txc.grant.phase": "request", "_txc.grant.tenant": tenantSlug,
		"_txc.grant.kind": "secret", "_txc.grant.name": "DB_DSN", "_txc.grant.verb": "release",
		"_txc.grant.via": "exec", "_txc.grant.sandbox": "db-dsn", "_txc.grant.env": "DB_DSN", "_txc.grant.principal.id": "service:research",
		"_txc.grant.principal.kind": "service", "_txc.grant.grant": g.ID, "_txc.grant.run": "task-1",
		"_txc.grant.stack": "web", "_txc.grant.workspace": "bench", "_txc.grant.node.class": "unreviewed",
		"_txc.grant.secret.pull": "none", "_txc.grant.secret.scope": "tenant", "_txc.grant.proposed.reason": "pull",
	} {
		if got := gjson.Get(env, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	for path, want := range map[string]bool{
		"_txc.grant.checks.exists": true, "_txc.grant.checks.allowlist": true, "_txc.grant.checks.standing": true,
		"_txc.grant.checks.pull": false, "_txc.grant.checks.budget": true, "_txc.grant.proposed.allow": false,
	} {
		if got := gjson.Get(env, path); got.Type != gjson.True && got.Type != gjson.False || got.Bool() != want {
			t.Errorf("%s = %s, want %v", path, got.Raw, want)
		}
	}
	for path, want := range map[string]int64{
		"_txc.grant.generation": 1, "_txc.grant.depth": 0, "_txc.grant.secret.version": 1,
		"_txc.grant.budget.calls": 5, "_txc.grant.budget.spent": 0,
	} {
		if got := gjson.Get(env, path); got.Type != gjson.Number || got.Int() != want {
			t.Errorf("%s = %s, want %d", path, got.Raw, want)
		}
	}
	if gjson.Get(env, "_txc.grant.res").Exists() {
		t.Errorf("the request carries a verdict nobody gave: %s", env)
	}
	if rid := gjson.Get(env, "_txc.rid").String(); rid == "" {
		t.Error("the request has no rid")
	}
}

func TestAnOpenPolicyReleasesAndCharges(t *testing.T) {
	r := newRig(t)
	g := r.ready(secrets.PullAny)

	wantValue(t, "pull any", r.ask(g, "DB_DSN"), "postgres://secret-value")
	if got := r.spent(g); got != 1 {
		t.Errorf("spent %d calls, want 1", got)
	}
	if !gjson.Get(r.seen[0], "_txc.grant.proposed.allow").Bool() || gjson.Get(r.seen[0], "_txc.grant.proposed.reason").Exists() {
		t.Errorf("proposal: %s", gjson.Get(r.seen[0], "_txc.grant.proposed").Raw)
	}
	// Every release is decided afresh, and charged.
	wantValue(t, "again", r.ask(g, "DB_DSN"), "postgres://secret-value")
	if r.runs != 2 || r.spent(g) != 2 {
		t.Errorf("after two releases: %d runs, %d spent", r.runs, r.spent(g))
	}
	if got := gjson.Get(r.seen[1], "_txc.grant.budget.spent").Int(); got != 1 {
		t.Errorf("the second request saw %d spent, want 1", got)
	}
	// The launcher presents a token; it is the same request.
	wantValue(t, "by token", r.open(g, "db-dsn"), "postgres://secret-value")
	last := r.seen[len(r.seen)-1]
	if gjson.Get(last, "_txc.grant.via").String() != "launcher" || gjson.Get(last, "_txc.grant.sandbox").String() != "db-dsn" {
		t.Errorf("the launcher's request: %s", gjson.Get(last, "_txc.grant").Raw)
	}
}

// The `_grant` run is an inlet of its own. Opened from inside another run —
// the exec's — it must not inherit that run's pins, or the `_sys` boot
// ladder finds no ops and no rule ever runs.
func TestTheGrantRunHasAContextOfItsOwn(t *testing.T) {
	r := newRig(t)
	g := r.ready(secrets.PullAny)
	ctx := processor.WithStack(processor.WithTenant(context.Background(), "acme"), "web/canary")
	wantValue(t, "from a pinned context", r.g.OpenSandbox(ctx, Open{TenantID: tenantID, GrantID: g.ID, Sandbox: "db-dsn", Via: ViaExec}), "postgres://secret-value")
	if len(r.pins) != 1 || r.pins[0] != "/" {
		t.Errorf("the _grant run was pinned to %q", r.pins)
	}
	// It still ends when the caller gives up.
	r.g.timeout = time.Minute
	r.g.runStack = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	gone, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	done := make(chan Answer, 1)
	go func() {
		done <- r.g.OpenSandbox(gone, Open{TenantID: tenantID, GrantID: g.ID, Sandbox: "db-dsn", Via: ViaExec})
	}()
	select {
	case a := <-done:
		wantRefused(t, "the caller gave up", a, reasonError)
	case <-time.After(5 * time.Second):
		t.Fatal("the run outlived the caller")
	}
}

// A sandbox opens whole or not at all: each secret is its own request, and
// one refusal hands nothing over.
func TestASandboxOpensWholeOrNotAtAll(t *testing.T) {
	r := newRig(t)
	r.store("A", "a-value", secrets.PullAny)
	r.store("B", "b-value", secrets.PullAny)
	r.store("C", "c-value", secrets.PullNone)
	r.hold("A", "B", "C")
	g := r.mint("task-1", func(n *authn.NewRunGrant) {
		n.Sandboxes = map[string]map[string]string{
			"both":   {"FIRST": "secret:A", "SECOND": "secret:B", "AGAIN": "secret:A"},
			"closed": {"FIRST": "secret:A", "THIRD": "secret:C"},
		}
	})

	a := r.ask(g, "both")
	if !a.Allowed || string(a.Env["FIRST"]) != "a-value" || string(a.Env["SECOND"]) != "b-value" || string(a.Env["AGAIN"]) != "a-value" || len(a.Env) != 3 {
		t.Fatalf("both: %+v", a)
	}
	// Three variables, three requests, three charges — in variable order.
	if r.runs != 3 || r.spent(g) != 3 {
		t.Errorf("%d runs, %d spent, want 3 and 3", r.runs, r.spent(g))
	}
	for i, want := range []string{"AGAIN", "FIRST", "SECOND"} {
		if got := gjson.Get(r.seen[i], "_txc.grant.env").String(); got != want || gjson.Get(r.seen[i], "_txc.grant.sandbox").String() != "both" {
			t.Errorf("request %d: env=%q sandbox=%q", i, got, gjson.Get(r.seen[i], "_txc.grant.sandbox").String())
		}
	}

	// C's policy refuses: FIRST was released and charged, and is not handed over.
	r.g.seen = newRefusals(0)
	wantRefused(t, "closed", r.ask(g, "closed"), reasonPull)
	if r.spent(g) != 4 {
		t.Errorf("spent %d, want 4: the release before the refusal stays charged", r.spent(g))
	}
	// A sandbox the grant does not name: refused before any rule runs.
	before := r.runs
	wantRefused(t, "unnamed", r.ask(g, "other"), reasonSandbox)
	wantRefused(t, "no name", r.g.OpenSandbox(context.Background(), Open{TenantID: tenantID, GrantID: g.ID, Via: ViaExec}), reasonSandbox)
	if r.runs != before {
		t.Errorf("the stack ran for a sandbox the grant does not name")
	}
}

func TestTheNodeClassAgainstThePolicy(t *testing.T) {
	r := newRig(t)
	r.store("DB_DSN", "v", secrets.PullReviewed)
	r.hold("DB_DSN")
	unreviewed := r.mint("unreviewed", nil, "DB_DSN")
	reviewed := r.mint("reviewed", func(n *authn.NewRunGrant) { n.NodeClass = authn.NodeReviewed }, "DB_DSN")

	wantRefused(t, "a reviewed-only secret on an unreviewed node", r.ask(unreviewed, "DB_DSN"), reasonPull)
	wantValue(t, "the same secret on a reviewed node", r.ask(reviewed, "DB_DSN"), "v")
	if got := gjson.Get(r.seen[1], "_txc.grant.node.class").String(); got != "reviewed" {
		t.Errorf("node class = %q", got)
	}
}

// The stack disposes: a rule may change the chassis's answer either way.
func TestARuleChangesTheAnswerEitherWay(t *testing.T) {
	r := newRig(t)
	r.store("CLOSED", "closed-value", secrets.PullNone)
	r.store("OPEN", "open-value", secrets.PullAny)
	r.hold("CLOSED", "OPEN")
	g := r.mint("task-1", nil, "CLOSED", "OPEN")

	r.rules = rule("_txc.grant.res.allow", true, "_txc.grant.res.reason", "this run is trusted")
	wantValue(t, "a rule allows what the policy refused", r.ask(g, "CLOSED"), "closed-value")
	if gjson.Get(r.seen[0], "_txc.grant.proposed.allow").Bool() {
		t.Error("the proposal was to allow; the test proves nothing")
	}

	r.rules = rule("_txc.grant.res.allow", false)
	wantRefused(t, "a rule refuses what the policy allowed", r.ask(g, "OPEN"), reasonRule)
	if !gjson.Get(r.seen[1], "_txc.grant.proposed.allow").Bool() {
		t.Error("the proposal was to refuse; the test proves nothing")
	}
	if got := r.spent(g); got != 1 {
		t.Errorf("spent %d calls, want 1: the release, not the refusal", got)
	}
}

func TestARuleCannotReleaseWhatIsNotThere(t *testing.T) {
	r := newRig(t)
	r.hold("MISSING")
	g := r.mint("task-1", nil, "MISSING")

	wantRefused(t, "no such secret", r.ask(g, "MISSING"), reasonNotFound)
	if r.runs != 1 || gjson.Get(r.seen[0], "_txc.grant.checks.exists").Bool() ||
		gjson.Get(r.seen[0], "_txc.grant.secret").Exists() {
		t.Errorf("a missing secret is presented, as missing: runs=%d %s", r.runs, r.seen)
	}
	r.now = r.now.Add(time.Minute) // past the refusal's memory
	r.rules = rule("_txc.grant.res.allow", true)
	wantRefused(t, "no such secret, and a rule that says yes", r.ask(g, "MISSING"), reasonNotFound)
	if got := r.spent(g); got != 0 {
		t.Errorf("spent %d calls on a secret that is not there", got)
	}
	// A revoked secret is not there either.
	r.store("GONE", "v", secrets.PullAny)
	r.hold("GONE")
	g = r.mint("task-2", nil, "GONE")
	if err := r.secrets.RevokeSecret(context.Background(), tenantID, nil, "GONE"); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, "a revoked secret", r.ask(g, "GONE"), reasonNotFound)
}

func TestEachCheckOfTheProposal(t *testing.T) {
	r := newRig(t)
	for _, n := range []string{"NAMED", "UNNAMED", "UNHELD"} {
		r.store(n, "value-of-"+n, secrets.PullAny)
	}
	r.hold("NAMED", "UNNAMED", "UNHELD")
	g := r.mint("task-1", nil, "NAMED", "UNHELD")
	// The principal loses a standing grant after the run was minted.
	if _, _, err := r.ids.RevokeGrantOn(context.Background(), tenantID, "web", r.who, authn.ResourceSecret, "UNHELD"); err != nil {
		t.Fatal(err)
	}

	wantValue(t, "named and held", r.ask(g, "NAMED"), "value-of-NAMED")
	wantRefused(t, "a sandbox the run does not name", r.ask(g, "UNNAMED"), reasonSandbox)
	wantRefused(t, "the standing grant was revoked", r.ask(g, "UNHELD"), reasonStanding)
	// The allowlist is derived from the sandboxes, so it can only disagree
	// with them when the row was changed under us. Then it wins.
	if _, err := r.ids.DB.ExecContext(context.Background(), r.ids.Dialect.Rebind(`UPDATE run_grants SET allowlist = ? WHERE id = ?`), `["secret:UNHELD"]`, g.ID); err != nil {
		t.Fatal(err)
	}
	r.g.seen = newRefusals(0)
	wantRefused(t, "not in the run's allowlist", r.ask(g, "NAMED"), reasonAllowlist)
	if _, err := r.ids.DB.ExecContext(context.Background(), r.ids.Dialect.Rebind(`UPDATE run_grants SET allowlist = ? WHERE id = ?`), `["secret:NAMED","secret:UNHELD"]`, g.ID); err != nil {
		t.Fatal(err)
	}
	// A disabled user holds nothing, whatever the rows say.
	u, _, _, err := r.ids.CreateUser(context.Background(), tenantID, "web", authn.NewUser{Email: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ids.PutGrant(context.Background(), tenantID, "web", u.Principal(), authn.NewGrant{Kind: authn.ResourceSecret, Name: "NAMED"}); err != nil {
		t.Fatal(err)
	}
	ug, err := r.ids.MintRunGrant(context.Background(), tenantID, "web", u.Principal(),
		authn.NewRunGrant{Run: "alice", Stack: "web", Sandboxes: map[string]map[string]string{"named": {"NAMED": "secret:NAMED"}}, BudgetCalls: 5, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	wantValue(t, "a user's run", r.ask(ug, "NAMED"), "value-of-NAMED")
	if _, err := r.ids.SetUserDisabled(context.Background(), tenantID, "web", u.ID, true); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, "a disabled user's run", r.ask(ug, "NAMED"), reasonStanding)

	// What a sandbox may reference was checked when the row was written;
	// a row that changed under us is an error at read, never a request.
	for name, sandboxes := range map[string]string{
		"a capability":        `{"named":{"NAMED":"capability:crm.lookup"}}`,
		"an unknown kind":     `{"named":{"NAMED":"drive:dc_1"}}`,
		"no name":             `{"named":{"NAMED":"secret:"}}`,
		"a path":              `{"named":{"NAMED":"secret:../etc/passwd"}}`,
		"a name with a dot":   `{"named":{"NAMED":"secret:db.dsn"}}`,
		"a name with a slash": `{"named":{"NAMED":"secret:a/b"}}`,
	} {
		if _, err := r.ids.DB.ExecContext(context.Background(), r.ids.Dialect.Rebind(`UPDATE run_grants SET sandboxes = ? WHERE id = ?`), sandboxes, g.ID); err != nil {
			t.Fatal(err)
		}
		before := r.runs
		a := r.ask(g, "NAMED")
		if a.Allowed || !a.Unavailable || len(a.Env) != 0 {
			t.Errorf("%s: %+v, want unavailable", name, a)
		}
		if r.runs != before {
			t.Errorf("%s: the stack ran for something that is not a request", name)
		}
	}
}

func TestTheBudget(t *testing.T) {
	r := newRig(t)
	r.store("DB_DSN", "v", secrets.PullAny)
	r.hold("DB_DSN")
	g := r.mint("task-1", func(n *authn.NewRunGrant) { n.BudgetCalls = 2 }, "DB_DSN")

	wantValue(t, "the first call", r.ask(g, "DB_DSN"), "v")
	wantValue(t, "the last call", r.ask(g, "DB_DSN"), "v")
	wantRefused(t, "one past the budget", r.ask(g, "DB_DSN"), reasonBudget)
	last := r.seen[len(r.seen)-1]
	if gjson.Get(last, "_txc.grant.checks.budget").Bool() || gjson.Get(last, "_txc.grant.budget.spent").Int() != 2 {
		t.Errorf("the over-budget request: %s", gjson.Get(last, "_txc.grant").Raw)
	}
	if got := r.spent(g); got != 2 {
		t.Errorf("a refusal spent: %d", got)
	}

	// A rule may say go ahead regardless. The spend is still recorded.
	r.now = r.now.Add(time.Minute)
	r.rules = rule("_txc.grant.res.allow", true)
	wantValue(t, "over budget, and a rule that allows it", r.ask(g, "DB_DSN"), "v")
	if got := r.spent(g); got != 3 {
		t.Errorf("spent %d, want 3: a rule's release is charged too", got)
	}
	// A rule that allows a request WITHIN budget does not lift the budget.
	g2 := r.mint("task-2", func(n *authn.NewRunGrant) { n.BudgetCalls = 1 }, "DB_DSN")
	wantValue(t, "within budget, by rule", r.ask(g2, "DB_DSN"), "v")
	if got := r.spent(g2); got != 1 {
		t.Errorf("spent %d, want 1", got)
	}
}

// Whatever was proposed, a run that did not decide refuses.
func TestARunThatDoesNotDecideRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		rules func(string) (string, error)
		want  string
	}{
		"the run fails":            {func(string) (string, error) { return "", errors.New("pipeline error: boom") }, reasonError},
		"admission is refused":     {rule("_txc.admission.denied", true, "_txc.admission.reason", "rate_limited"), reasonAdmission},
		"routing is unavailable":   {rule("_txc.route.unavailable", true), reasonRoute},
		"a rule holds it":          {rule("_txc.grant.res.hold", true), reasonHeld},
		"a hold beats an allow":    {rule("_txc.grant.res.hold", true, "_txc.grant.res.allow", true), reasonHeld},
		"allow is the string true": {rule("_txc.grant.res.allow", "true"), reasonVerdict},
		"allow is the number 1":    {rule("_txc.grant.res.allow", 1), reasonVerdict},
		"allow is an object":       {rule("_txc.grant.res.allow.yes", true), reasonVerdict},
		"hold is a string":         {rule("_txc.grant.res.hold", "yes"), reasonVerdict},
		"admission beats an allow": {rule("_txc.admission.denied", true, "_txc.grant.res.allow", true), reasonAdmission},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			g := r.ready(secrets.PullAny) // the proposal is to allow
			r.rules = tc.rules
			wantRefused(t, name, r.ask(g, "DB_DSN"), tc.want)
			if got := r.spent(g); got != 0 {
				t.Errorf("spent %d calls", got)
			}
		})
	}

	t.Run("the run does not answer in time", func(t *testing.T) {
		r := newRig(t)
		g := r.ready(secrets.PullAny)
		r.g.timeout = 20 * time.Millisecond
		r.g.runStack = func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}
		wantRefused(t, "timeout", r.ask(g, "DB_DSN"), reasonTimeout)
	})

	// An allow that is absent or null is no verdict: the proposal stands.
	for name, rules := range map[string]func(string) (string, error){
		"a reason and no verdict": rule("_txc.grant.res.reason", "looked fine"),
		"allow is null":           rule("_txc.grant.res.allow", nil),
		"hold is false":           rule("_txc.grant.res.hold", false),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			g := r.ready(secrets.PullAny)
			r.rules = rules
			wantValue(t, name, r.ask(g, "DB_DSN"), "postgres://secret-value")
		})
	}
}

// The checks that are the chassis's alone: no rule runs, and none can help.
func TestHardChecksRunNoStack(t *testing.T) {
	r := newRig(t)
	g := r.ready(secrets.PullAny)
	r.rules = rule("_txc.grant.res.allow", true) // a tenant that allows everything
	ctx := context.Background()
	ask := func(o Open) Answer {
		o.Sandbox, o.Via = "db-dsn", ViaLauncher
		return r.g.OpenSandbox(ctx, o)
	}

	good := r.token(g)
	parts := strings.Split(good, ".")
	other, _ := rungrant.New(bytes.Repeat([]byte{9}, 32), 1)
	forged, _ := other.Sign(rungrant.Claims{Grant: g.ID, Tenant: g.TenantID, Principal: g.Principal.ID,
		Run: g.Run, Generation: g.Generation, Expires: g.ExpiresAt.Unix()})
	// A forger who rewrites the claims has only the old MAC to put under them.
	rewritten, _ := other.Sign(rungrant.Claims{Grant: g.ID, Tenant: g.TenantID, Principal: g.Principal.ID,
		Run: g.Run, Generation: g.Generation, Expires: g.ExpiresAt.Unix() + 86400})
	sign := func(mut func(*rungrant.Claims)) string {
		c := rungrant.Claims{Grant: g.ID, Tenant: g.TenantID, Principal: g.Principal.ID,
			Run: g.Run, Generation: g.Generation, Expires: g.ExpiresAt.Unix()}
		mut(&c)
		tok, err := r.signer.Sign(c)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}

	for name, tc := range map[string]struct {
		open Open
		want string
	}{
		"nothing presented":              {Open{}, reasonToken},
		"garbage":                        {Open{Token: "not-a-token"}, reasonToken},
		"signed by another key":          {Open{Token: forged}, reasonToken},
		"claims rewritten, the MAC kept": {Open{Token: parts[0] + "." + strings.Split(rewritten, ".")[1] + "." + parts[2]}, reasonToken},
		"a token and an id":              {Open{Token: good, TenantID: tenantID, GrantID: g.ID}, reasonToken},
		"an id of no grant":              {Open{TenantID: tenantID, GrantID: "rgr_missing"}, reasonNotLive},
		"an id under another tenant":     {Open{TenantID: "tnt_other", GrantID: g.ID}, reasonNotLive},
		"an id under no tenant":          {Open{GrantID: g.ID}, reasonNotLive},
		"a grant that does not exist":    {Open{Token: sign(func(c *rungrant.Claims) { c.Grant = "rgr_missing" })}, reasonNotLive},
		"another tenant's claim":         {Open{Token: sign(func(c *rungrant.Claims) { c.Tenant = "tnt_other" })}, reasonNotLive},
		"another generation":             {Open{Token: sign(func(c *rungrant.Claims) { c.Generation = 2 })}, reasonGeneration},
		"another principal":              {Open{Token: sign(func(c *rungrant.Claims) { c.Principal = "service:admin" })}, reasonGeneration},
		"another run":                    {Open{Token: sign(func(c *rungrant.Claims) { c.Run = "task-2" })}, reasonGeneration},
		"a token that expired early":     {Open{Token: sign(func(c *rungrant.Claims) { c.Expires = r.now.Unix() - 1 })}, reasonExpired},
	} {
		wantRefused(t, name, ask(tc.open), tc.want)
	}
	if r.runs != 0 || r.spent(g) != 0 {
		t.Fatalf("hard checks ran the stack %d times and spent %d", r.runs, r.spent(g))
	}
	wantValue(t, "the genuine token", ask(Open{Token: good}), "postgres://secret-value")

	// The row ends; the token is still genuine, and still refused.
	for name, end := range map[string]func(authn.RunGrant){
		"revoked": func(g authn.RunGrant) { _, _, _ = r.ids.RevokeRunGrant(ctx, tenantID, "web", g.ID) },
		"closed":  func(g authn.RunGrant) { _, _, _ = r.ids.CloseRunGrant(ctx, tenantID, "web", g.ID, "done") },
		"minted again": func(g authn.RunGrant) {
			r.mint(g.Run, nil, "DB_DSN")
		},
		"expired": func(authn.RunGrant) { r.now = r.now.Add(11 * time.Minute) },
	} {
		g := r.mint("ends-"+strings.ReplaceAll(name, " ", "-"), nil, "DB_DSN")
		tok := r.token(g)
		wantValue(t, name+": before", ask(Open{Token: tok}), "postgres://secret-value")
		before := r.runs
		end(g)
		want := reasonNotLive
		if name == "expired" {
			want = reasonExpired // the token's own expiry is the grant's
		}
		wantRefused(t, name+": by token", ask(Open{Token: tok}), want)
		wantRefused(t, name+": by id", ask(Open{TenantID: tenantID, GrantID: g.ID}), reasonNotLive)
		if r.runs != before {
			t.Errorf("%s: the stack ran for a grant that does not hold", name)
		}
	}

	// A tenant that is gone.
	r2 := newRig(t)
	g2 := r2.ready(secrets.PullAny)
	r2.g.tenantSlug = func(context.Context, string) (string, error) { return "", errNoTenant }
	wantRefused(t, "the tenant is gone", r2.ask(g2, "DB_DSN"), reasonTenant)
	if r2.runs != 0 {
		t.Error("the stack of a tenant that is gone ran")
	}
}

func TestWhatTheChassisCannotAnswerIsNotARefusal(t *testing.T) {
	boom := errors.New("connection refused")
	for name, breakIt := range map[string]func(*rig){
		"the tenant directory": func(r *rig) {
			r.g.tenantSlug = func(context.Context, string) (string, error) { return "", boom }
		},
		"the secret store": func(r *rig) {
			r.g.resolve = func(context.Context, string, string, string) (*secrets.SecretMetadata, error) { return nil, boom }
		},
		"the decrypt": func(r *rig) {
			r.g.decrypt = func(context.Context, *secrets.SecretMetadata) ([]byte, error) { return nil, boom }
		},
		"the identity store": func(r *rig) { _ = r.ids.DB.Close() },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			g := r.ready(secrets.PullAny)
			breakIt(r)
			a := r.ask(g, "DB_DSN")
			if a.Allowed || !a.Unavailable || len(a.Env) != 0 {
				t.Errorf("%+v, want unavailable", a)
			}
		})
	}
	// No signing key: a token cannot be checked; the exec's own row still can.
	r := newRig(t)
	g := r.ready(secrets.PullAny)
	tok := r.token(g)
	r.g.signer = nil
	if a := r.g.OpenSandbox(context.Background(), Open{Token: tok, Sandbox: "db-dsn"}); a.Allowed || !a.Unavailable {
		t.Errorf("a token with no key to check it: %+v", a)
	}
	wantValue(t, "the row needs no signing key", r.ask(g, "DB_DSN"), "postgres://secret-value")
}

func TestARefusalIsRememberedBriefly(t *testing.T) {
	r := newRig(t)
	g := r.ready(secrets.PullNone)
	other := r.mint("task-2", nil, "DB_DSN")

	for i := 0; i < 50; i++ {
		wantRefused(t, "asked again", r.ask(g, "DB_DSN"), reasonPull)
	}
	if r.runs != 1 {
		t.Errorf("a program asking 50 times ran the stack %d times", r.runs)
	}
	// Another run's request is its own.
	wantRefused(t, "another run", r.ask(other, "DB_DSN"), reasonPull)
	if r.runs != 2 {
		t.Errorf("another run's request was answered from this one's memory: %d runs", r.runs)
	}
	// The operator opens the secret. Within the window the refusal stands…
	if _, err := r.secrets.UpdateSecretPull(context.Background(), tenantID, nil, "DB_DSN", secrets.PullAny); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(4 * time.Second)
	wantRefused(t, "inside the window", r.ask(g, "DB_DSN"), reasonPull)
	// …and past it the request is decided afresh.
	r.now = r.now.Add(2 * time.Second)
	wantValue(t, "past the window", r.ask(g, "DB_DSN"), "postgres://secret-value")
	// A release is never remembered: closing the secret takes effect at once.
	if _, err := r.secrets.UpdateSecretPull(context.Background(), tenantID, nil, "DB_DSN", secrets.PullNone); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, "closed again", r.ask(g, "DB_DSN"), reasonPull)

	// Turned off, every request runs the stack.
	r2 := newRig(t)
	g2 := r2.ready(secrets.PullNone)
	r2.g.seen = newRefusals(0)
	for i := 0; i < 3; i++ {
		r2.ask(g2, "DB_DSN")
	}
	if r2.runs != 3 {
		t.Errorf("with no memory: %d runs, want 3", r2.runs)
	}
}

// Nothing a rule should not hold is in what it is shown.
func TestTheRequestCarriesNoSecretNoTokenNoFileKey(t *testing.T) {
	r := newRig(t)
	g := r.ready(secrets.PullAny)
	tok := r.token(g)
	r.g.OpenSandbox(context.Background(), Open{Token: tok, Sandbox: "db-dsn", Via: ViaLauncher})
	r.ask(g, "DB_DSN")
	if len(r.seen) != 2 {
		t.Fatalf("%d requests presented", len(r.seen))
	}
	for _, env := range r.seen {
		for what, leak := range map[string]string{
			"the secret's value": "postgres://secret-value", "the token": tok,
			"the token's tag": rungrant.Prefix, "the file key": g.FileKey,
		} {
			if strings.Contains(env, leak) {
				t.Errorf("the request holds %s: %s", what, env)
			}
		}
	}
}

func TestRacingForTheLastCallReleasesOnce(t *testing.T) {
	r := newRig(t)
	r.store("DB_DSN", "v", secrets.PullAny)
	r.hold("DB_DSN")
	g := r.mint("task-1", func(n *authn.NewRunGrant) { n.BudgetCalls = 3 }, "DB_DSN")
	r.g.seen = newRefusals(0)

	var released atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a := r.ask(g, "DB_DSN"); a.Allowed {
				released.Add(1)
			} else if a.Reason != reasonBudget {
				t.Errorf("refused for %q", a.Reason)
			}
		}()
	}
	wg.Wait()
	if released.Load() != 3 || r.spent(g) != 3 {
		t.Errorf("released %d times, spent %d, of a budget of 3", released.Load(), r.spent(g))
	}
}
