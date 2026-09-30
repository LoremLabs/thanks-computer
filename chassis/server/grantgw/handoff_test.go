package grantgw

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/rungrant"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

func TestHandingAGrantToACommand(t *testing.T) {
	r := newRig(t)
	r.store("DB_DSN", "postgres://secret-value", secrets.PullAny)
	r.store("GITHUB_PAT", "ghp_secret", secrets.PullAny)
	r.store("CLOSED", "closed-value", secrets.PullNone)
	r.hold("DB_DSN", "GITHUB_PAT", "CLOSED")
	g := r.mint("task-1", func(n *authn.NewRunGrant) {
		n.Sandboxes = map[string]map[string]string{
			"postgres": {"PGDSN": "secret:DB_DSN"},
			"github":   {"GH_TOKEN": "secret:GITHUB_PAT", "GH_USER_TOKEN": "secret:GITHUB_PAT"},
			"closed":   {"CLOSED": "secret:CLOSED"},
			"clash":    {"PGDSN": "secret:GITHUB_PAT"},
		}
	})
	r.g.ServeSocket("/tmp/txco-1/g.sock")
	ctx := context.Background()

	// The push form: the sandboxes are opened here, and the command gets
	// their variables; with the socket on, what it needs to open more.
	env, scrub, err := r.g.ForExec(ctx, tenantSlug, "web/_mail", "ws_bench", g.ID, []string{"github", "postgres", "github"})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"PGDSN": "postgres://secret-value", "GH_TOKEN": "ghp_secret", "GH_USER_TOKEN": "ghp_secret",
		EnvRun: "task-1", EnvSocket: "/tmp/txco-1/g.sock", EnvBin: "/usr/local/bin/txco",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if len(env) != 7 {
		t.Errorf("env = %v", env)
	}
	// Each secret was a request of its own, via exec, and charged.
	if r.runs != 3 || r.spent(g) != 3 {
		t.Errorf("%d runs, %d spent, want 3 and 3", r.runs, r.spent(g))
	}
	for i, want := range [][2]string{{"github", "GH_TOKEN"}, {"github", "GH_USER_TOKEN"}, {"postgres", "PGDSN"}} {
		if gjson.Get(r.seen[i], "_txc.grant.via").String() != "exec" || gjson.Get(r.seen[i], "_txc.grant.sandbox").String() != want[0] ||
			gjson.Get(r.seen[i], "_txc.grant.env").String() != want[1] {
			t.Errorf("request %d: %s", i, gjson.Get(r.seen[i], "_txc.grant").Raw)
		}
	}
	// The token is this grant's, and good for exactly as long as the grant.
	claims, err := r.signer.Verify(env[EnvToken], r.now)
	if err != nil || claims.Grant != g.ID || claims.Tenant != tenantID || claims.Principal != "service:research" ||
		claims.Run != "task-1" || claims.Generation != 1 || claims.Expires != g.ExpiresAt.Unix() {
		t.Errorf("claims = %+v err=%v", claims, err)
	}
	if _, err := r.signer.Verify(env[EnvToken], g.ExpiresAt); !errors.Is(err, rungrant.ErrExpired) {
		t.Errorf("the token outlives the grant: %v", err)
	}
	// …and the gateway takes it.
	wantValue(t, "the handed token", r.g.OpenSandbox(ctx, Open{Token: env[EnvToken], Sandbox: "postgres", Via: ViaLauncher}), "postgres://secret-value")

	// What must never appear in the command's output: every value, the token.
	var scrubbed []string
	for _, s := range scrub {
		scrubbed = append(scrubbed, string(s))
	}
	got := strings.Join(scrubbed, " ")
	for _, want := range []string{env[EnvToken], "postgres://secret-value", "ghp_secret"} {
		if !strings.Contains(got, want) {
			t.Errorf("scrub list lacks %q", want)
		}
	}

	// A refused sandbox: nothing is handed over, the error names the
	// sandbox and not why, and what was released before stays charged.
	spent := r.spent(g)
	env, scrub, err = r.g.ForExec(ctx, tenantSlug, "web", "ws_bench", g.ID, []string{"postgres", "closed"})
	if !errors.Is(err, ErrHandoff) || !strings.Contains(err.Error(), `sandbox "closed" was refused`) || strings.Contains(err.Error(), "pull") || env != nil || scrub != nil {
		t.Errorf("a refused sandbox: %v %v err=%v", env, scrub, err)
	}
	if r.spent(g) != spent+1 {
		t.Errorf("spent %d, want %d: postgres was released before closed was refused", r.spent(g), spent+1)
	}

	for name, tc := range map[string]struct {
		tenant, stack, workspace, grant string
		sandboxes                       []string
		want                            string
	}{
		"another workspace":    {tenantSlug, "web", "ws_other", g.ID, []string{"postgres"}, `minted for workspace "bench"`},
		"another stack":        {tenantSlug, "ingest", "ws_bench", g.ID, []string{"postgres"}, "minted by another stack"},
		"another tenant":       {"other", "web", "ws_bench", g.ID, []string{"postgres"}, `tenant "other" not found`},
		"a grant that is not":  {tenantSlug, "web", "ws_bench", "rgr_missing", []string{"postgres"}, "no run grant"},
		"a file key for an id": {tenantSlug, "web", "ws_bench", g.FileKey, []string{"postgres"}, "no run grant"},
		"no grant":             {tenantSlug, "web", "ws_bench", "", []string{"postgres"}, "no run grant"},
		"no stack":             {tenantSlug, "", "ws_bench", g.ID, []string{"postgres"}, "reading run grant"},
		"an unnamed sandbox":   {tenantSlug, "web", "ws_bench", g.ID, []string{"postgres", "deploy"}, `does not name sandbox "deploy" (it names: clash, closed, github, postgres)`},
		"one variable twice":   {tenantSlug, "web", "ws_bench", g.ID, []string{"postgres", "clash"}, "both set PGDSN"},
	} {
		before := r.runs
		env, scrub, err := r.g.ForExec(ctx, tc.tenant, tc.stack, tc.workspace, tc.grant, tc.sandboxes)
		if !errors.Is(err, ErrHandoff) || !strings.Contains(err.Error(), tc.want) || env != nil || scrub != nil {
			t.Errorf("%s: env=%v err=%v, want %q", name, env, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), rungrant.Prefix) || strings.Contains(err.Error(), g.FileKey) && name != "a file key for an id") {
			t.Errorf("%s: the refusal names a token or a file key: %v", name, err)
		}
		if r.runs != before {
			t.Errorf("%s: something was asked for before the handoff was refused", name)
		}
	}

	// A grant minted for no workspace goes to no command.
	bare := r.mint("bare", func(n *authn.NewRunGrant) { n.Workspace, n.WorkspaceID = "", "" }, "DB_DSN")
	if _, _, err := r.g.ForExec(ctx, tenantSlug, "web", "", bare.ID, []string{"db-dsn"}); err == nil || !strings.Contains(err.Error(), "minted for no workspace") {
		t.Errorf("a grant with no workspace: %v", err)
	}
	// One that ended goes nowhere.
	if _, _, err := r.ids.RevokeRunGrant(ctx, tenantID, "web", g.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.g.ForExec(ctx, tenantSlug, "web", "ws_bench", g.ID, []string{"postgres"}); err == nil || !strings.Contains(err.Error(), "expired, ended or been revoked") {
		t.Errorf("a revoked grant: %v", err)
	}
}

func TestHandoffWithoutTheSocket(t *testing.T) {
	r := newRig(t)
	g := r.ready(secrets.PullAny)
	ctx := context.Background()

	// No socket: the push form still works, and the command gets the
	// sandbox's variables and nothing of the chassis's.
	env, scrub, err := r.g.ForExec(ctx, tenantSlug, "web", "ws_bench", g.ID, []string{"db-dsn"})
	if err != nil || env["DB_DSN"] != "postgres://secret-value" || len(env) != 1 || len(scrub) != 1 {
		t.Errorf("the push form alone: %v %v err=%v", env, scrub, err)
	}
	// No socket and no sandbox: nothing would be handed over.
	if _, _, err := r.g.ForExec(ctx, tenantSlug, "web", "ws_bench", g.ID, nil); err == nil || !strings.Contains(err.Error(), "--personalities") {
		t.Errorf("with neither the socket nor a sandbox: %v", err)
	}
	// The socket alone: the command gets what it needs to open sandboxes
	// itself, and no value.
	r.g.ServeSocket("/tmp/g.sock")
	env, scrub, err = r.g.ForExec(ctx, tenantSlug, "web", "ws_bench", g.ID, nil)
	if err != nil || env[EnvSocket] != "/tmp/g.sock" || env[EnvRun] != "task-1" || env[EnvToken] == "" || len(env) != 4 || len(scrub) != 1 {
		t.Errorf("the socket alone: %v err=%v", env, err)
	}
	if r.runs != 1 {
		t.Errorf("the socket alone asked for something: %d runs", r.runs)
	}

	r.g.signer = nil
	if _, _, err := r.g.ForExec(ctx, tenantSlug, "web", "ws_bench", g.ID, []string{"db-dsn"}); err == nil || !strings.Contains(err.Error(), "master key") {
		t.Errorf("with no signing key: %v", err)
	}
}

// fakeBus plays the processor's end of the bus.
func fakeBus(t *testing.T, life context.Context, answer func(*event.Envelope)) chan<- *event.Envelope {
	t.Helper()
	bus := make(chan *event.Envelope)
	go func() {
		for {
			select {
			case env := <-bus:
				go answer(env)
			case <-life.Done():
				return
			}
		}
	}()
	return bus
}

func TestRunOnBus(t *testing.T) {
	life, stop := context.WithCancel(context.Background())
	defer stop()
	ctx := context.Background()

	for name, tc := range map[string]struct {
		answer  func(*event.Envelope)
		want    string
		wantErr string
	}{
		"the final payload": {func(e *event.Envelope) {
			if e.Src != srcName {
				t.Errorf("src = %q", e.Src)
			}
			e.ResCh <- event.Payload{Raw: e.Payload.Raw + "!", Type: event.JSON}
		}, `{"a":1}!`, ""},
		"a pipeline error": {func(e *event.Envelope) {
			e.ResCh <- event.Payload{Raw: "boom", Type: event.ErrorStr}
		}, "", "pipeline error: boom"},
		"a rule that streams": {func(e *event.Envelope) {
			e.ResCh <- event.Payload{Type: event.StreamHead}
			e.ResCh <- event.Payload{Raw: "chunk", Type: event.StreamChunk}
			e.ResCh <- event.Payload{Type: event.StreamEnd}
		}, "", "streamed"},
	} {
		got, err := RunOnBus(ctx, life, fakeBus(t, life, tc.answer), `{"a":1}`)
		if got != tc.want || (err == nil) != (tc.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("%s: %q err=%v", name, got, err)
		}
	}
	if _, err := RunOnBus(ctx, life, nil, `{}`); err == nil {
		t.Error("no bus, no error")
	}

	// A run that answers after the wait was abandoned must still be able
	// to send: the processor sends without a select.
	sent := make(chan struct{})
	slow := fakeBus(t, life, func(e *event.Envelope) {
		<-e.Ctx.Done()
		for i := 0; i < 20; i++ { // more than the channel buffers
			e.ResCh <- event.Payload{Raw: "chunk", Type: event.StreamChunk}
		}
		e.ResCh <- event.Payload{Raw: `{}`, Type: event.JSON}
		close(sent)
	})
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := RunOnBus(short, life, slow, `{}`); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an abandoned wait: %v", err)
	}
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Error("the run is stuck sending to a channel nobody reads")
	}

	// A bus nobody reads, and a chassis that is stopping.
	dead := make(chan *event.Envelope)
	short2, cancel2 := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel2()
	if _, err := RunOnBus(short2, life, dead, `{}`); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a bus nobody reads: %v", err)
	}
	stopped, stopNow := context.WithCancel(ctx)
	stopNow()
	if _, err := RunOnBus(ctx, stopped, dead, `{}`); !errors.Is(err, errShutdown) {
		t.Errorf("a chassis that is stopping: %v", err)
	}
}

func TestProposeAndDecide(t *testing.T) {
	all := checks{exists: true, allowlist: true, standing: true, pull: true, budget: true}
	if p := propose(all); !p.allow || p.reason != "" {
		t.Errorf("every check passes: %+v", p)
	}
	// The reason is the first check that failed, outermost first.
	for want, c := range map[string]checks{
		reasonNotFound:  {},
		reasonAllowlist: {exists: true},
		reasonStanding:  {exists: true, allowlist: true},
		reasonPull:      {exists: true, allowlist: true, standing: true},
		reasonBudget:    {exists: true, allowlist: true, standing: true, pull: true},
	} {
		if p := propose(c); p.allow || p.reason != want {
			t.Errorf("%+v: %+v, want %q", c, p, want)
		}
	}

	yes, no := proposal{allow: true}, proposal{reason: reasonPull}
	boom := errors.New("boom")
	for name, tc := range map[string]struct {
		p        proposal
		v        verdict
		err      error
		timedOut bool
		allow    bool
		reason   string
		byRule   bool
	}{
		"no verdict, proposed yes":  {p: yes, allow: true},
		"no verdict, proposed no":   {p: no, reason: reasonPull},
		"a rule allows":             {p: no, v: verdict{set: true, allow: true}, allow: true, byRule: true},
		"a rule refuses":            {p: yes, v: verdict{set: true}, reason: reasonRule, byRule: true},
		"a hold":                    {p: yes, v: verdict{hold: true}, reason: reasonHeld, byRule: true},
		"a hold beats an allow":     {p: yes, v: verdict{hold: true, set: true, allow: true}, reason: reasonHeld, byRule: true},
		"malformed beats an allow":  {p: yes, v: verdict{malformed: true, set: true, allow: true}, reason: reasonVerdict, byRule: true},
		"admission beats a rule":    {p: yes, v: verdict{admission: true, set: true, allow: true}, reason: reasonAdmission},
		"routing beats admission":   {p: yes, v: verdict{unavailable: true, admission: true}, reason: reasonRoute},
		"an error beats everything": {p: yes, v: verdict{set: true, allow: true}, err: boom, reason: reasonError},
		"a timeout is named as one": {p: yes, err: context.DeadlineExceeded, timedOut: true, reason: reasonTimeout},
	} {
		d := decide(tc.p, tc.v, tc.err, tc.timedOut)
		if d.allow != tc.allow || d.reason != tc.reason || d.byRule != tc.byRule {
			t.Errorf("%s: %+v", name, d)
		}
	}
}

func TestParseVerdict(t *testing.T) {
	for raw, want := range map[string]verdict{
		`{}`: {},
		`{"_txc":{"grant":{"proposed":{"allow":true}}}}`:              {}, // the proposal is not a verdict
		`{"_txc":{"grant":{"res":{"allow":true}}}}`:                   {set: true, allow: true},
		`{"_txc":{"grant":{"res":{"allow":false,"reason":"no"}}}}`:    {set: true, reason: "no"},
		`{"_txc":{"grant":{"res":{"allow":null}}}}`:                   {},
		`{"_txc":{"grant":{"res":{"allow":"true"}}}}`:                 {malformed: true},
		`{"_txc":{"grant":{"res":{"allow":1}}}}`:                      {malformed: true},
		`{"_txc":{"grant":{"res":{"allow":[true]}}}}`:                 {malformed: true},
		`{"_txc":{"grant":{"res":{"hold":true}}}}`:                    {hold: true},
		`{"_txc":{"grant":{"res":{"hold":false,"allow":true}}}}`:      {set: true, allow: true},
		`{"_txc":{"grant":{"res":{"hold":"soon"}}}}`:                  {malformed: true},
		`{"_txc":{"grant":{"res":{"reason":42}}}}`:                    {},
		`{"_txc":{"admission":{"denied":true,"reason":"suspended"}}}`: {admission: true, admReason: "suspended"},
		`{"_txc":{"route":{"unavailable":true}}}`:                     {unavailable: true},
		`{"grant":{"res":{"allow":true}}}`:                            {}, // the author's own key, not the control plane
		`not json`:                                                    {},
	} {
		if got := parseVerdict(raw); got != want {
			t.Errorf("%s: %+v, want %+v", raw, got, want)
		}
	}
	long := parseVerdict(`{"_txc":{"grant":{"res":{"allow":false,"reason":"` + strings.Repeat("x", 500) + `"}}}}`)
	if len(long.reason) != maxRuleReason {
		t.Errorf("a long reason is kept at %d bytes", len(long.reason))
	}
}
