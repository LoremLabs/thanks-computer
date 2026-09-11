package processor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// stubProvider is a workspace.Provider + Computer returning a canned
// result, standing in for a real backend so the processor wiring can be
// tested without spawning processes.
type stubProvider struct {
	res     workspace.ExecResult
	err     error
	echoEnv bool     // when set, stdout = the request env as KEY=VAL lines, stderr = "err:" + env["PLAIN"]
	stream  []string // when set and the request streams, written to StdoutTo in order

	mu       sync.Mutex
	seen     workspace.ExecRequest
	seenSpec workspace.Spec
	deadline time.Time
	execs    atomic.Int32
	destroys atomic.Int32
}

func (s *stubProvider) Name() string           { return "stub" }
func (s *stubProvider) Capabilities() []string { return []string{"exec"} }
func (s *stubProvider) Create(_ context.Context, spec workspace.Spec) (workspace.Handle, error) {
	s.mu.Lock()
	s.seenSpec = spec
	s.mu.Unlock()
	return workspace.Handle{Provider: "stub", Ref: "ref-" + spec.Name, Name: spec.Name}, nil
}
func (s *stubProvider) Wake(context.Context, workspace.Handle) (workspace.Computer, error) {
	return s, nil
}
func (s *stubProvider) Sleep(context.Context, workspace.Handle) error { return nil }
func (s *stubProvider) Destroy(context.Context, workspace.Handle) error {
	s.destroys.Add(1)
	return nil
}
func (s *stubProvider) Status(context.Context, workspace.Handle) (workspace.Status, error) {
	return workspace.Status{State: "running"}, nil
}
func (s *stubProvider) Checkpoint(_ context.Context, _ workspace.Handle, comment string) (string, error) {
	return "cp-" + comment, nil
}
func (s *stubProvider) Exec(ctx context.Context, req workspace.ExecRequest, _ workspace.Limits) (workspace.ExecResult, error) {
	s.execs.Add(1)
	s.mu.Lock()
	s.seen = req
	s.deadline, _ = ctx.Deadline()
	s.mu.Unlock()
	if s.err != nil {
		return workspace.ExecResult{Exit: -1}, s.err
	}
	if req.StdoutTo != nil {
		for _, chunk := range s.stream {
			if _, err := req.StdoutTo.Write([]byte(chunk)); err != nil {
				return workspace.ExecResult{Exit: -1}, err
			}
		}
		out := s.res
		for _, chunk := range s.stream {
			out.StdoutBytes += int64(len(chunk))
		}
		out.Stdout = nil
		return out, nil
	}
	if s.echoEnv {
		return workspace.ExecResult{Exit: 0, Stdout: []byte(strings.Join(workspace.SortedEnv(req.Env), "\n")), Stderr: []byte("err:" + req.Env["PLAIN"])}, nil
	}
	return s.res, nil
}

func workspaceOp(exec, meta string) operation.Operation {
	return operation.Operation{
		Stack:     "site",
		Scope:     100,
		Name:      "ws",
		Resonator: &resonator.Resonator{Exec: exec},
		Meta:      meta,
		Input:     `{}`,
	}
}

func newWorkspaceUnit(t *testing.T, stub *stubProvider) (*Unit, context.Context) {
	t.Helper()
	pu, _ := newTestUnit(t)
	pu.Workspaces = workspace.NewManager(stub, workspace.Limits{}, nil)
	return pu, WithTenant(context.Background(), "acme")
}

func TestExecWorkspaceNoManager(t *testing.T) {
	pu, _ := newTestUnit(t)
	ctx := WithTenant(context.Background(), "acme")
	if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"true"}`)); err == nil {
		t.Fatal("ExecWorkspace with nil manager = nil error, want loud failure")
	}
}

func TestExecWorkspaceMalformedRef(t *testing.T) {
	pu, ctx := newWorkspaceUnit(t, &stubProvider{})
	for _, ref := range []string{"workspace://tools", "workspace://tools/frobnicate", "workspace:///exec", "workspace://"} {
		if _, err := pu.ExecWorkspace(ctx, workspaceOp(ref, `{"command":"true"}`)); err == nil {
			t.Errorf("ref %q accepted", ref)
		}
	}
}

func TestExecWorkspaceUntenanted(t *testing.T) {
	pu, _ := newWorkspaceUnit(t, &stubProvider{})
	_, err := pu.ExecWorkspace(context.Background(), workspaceOp("workspace://tools/exec", `{"command":"true"}`))
	if err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("untenanted exec: err = %v, want tenant rejection", err)
	}
}

func TestExecWorkspaceBadNameAndOverride(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, ctx := newWorkspaceUnit(t, stub)
	for _, bad := range []string{"..", "a--b", "exec", "a/b/c/d/e", "Tools", "-x"} {
		meta := `{"command":"true","workspace":` + quoteJSON(bad) + `}`
		if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", meta)); err == nil {
			t.Errorf("WITH workspace = %q accepted", bad)
		}
	}
	// The static ref name is validated too.
	if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://Bad/exec", `{"command":"true"}`)); err != nil {
		// ParseRef accepts it; ValidateName must reject it.
	} else {
		t.Error("ref name \"Bad\" accepted")
	}
	// A valid override replaces the ref's name entirely.
	if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"true","workspace":"pony/paris"}`)); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	got := stub.seenSpec
	stub.mu.Unlock()
	if got.Name != "pony/paris" || got.Tenant != "acme" || got.Stack != "site" {
		t.Errorf("spec = %+v, want name pony/paris tenant acme stack site", got)
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestExecWorkspaceSecretsNonEnvRejectedInBand(t *testing.T) {
	pu, ctx := newWorkspaceUnit(t, &stubProvider{})
	for _, meta := range []string{
		`{"command":"true","secrets":{"headers":{"authorization":{"secret":"PONY_TOKEN"}}}}`,
		`{"command":"true","secrets":{"body":{"key":{"secret":"PONY_TOKEN"}}}}`,
		`{"command":"true","secrets":{"env":{"":{"secret":"PONY_TOKEN"}}}}`,
	} {
		p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", meta))
		if err != nil {
			t.Errorf("%s: dropped (%v), want in-band bad_request", meta, err)
			continue
		}
		if gjson.Get(p.Raw, "workspace.error.code").String() != "bad_request" {
			t.Errorf("%s: payload = %s", meta, p.Raw)
		}
	}
	// A malformed secrets block is an authoring error: the op is dropped.
	if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"true","secrets":"nope"}`)); err == nil {
		t.Error("malformed secrets block accepted")
	}
}

func TestExecWorkspaceRequestShape(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, ctx := newWorkspaceUnit(t, stub)
	meta := `{"command":"cat","stdin":"fed","cwd":"sub","env":{"A":"1","N":2}}`
	if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", meta)); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	req := stub.seen
	stub.mu.Unlock()
	if req.Command != "cat" || string(req.Stdin) != "fed" || req.Cwd != "sub" || req.Env["A"] != "1" || req.Env["N"] != "2" {
		t.Errorf("request = %+v", req)
	}
	// args mode
	if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"args":["/bin/echo","hi"]}`)); err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	req = stub.seen
	stub.mu.Unlock()
	if len(req.Args) != 2 || req.Args[1] != "hi" || req.Command != "" {
		t.Errorf("args request = %+v", req)
	}
	// Bad request shapes are in-band (the op still merges), not dropped.
	for _, meta := range []string{`{}`, `{"command":"x","args":["y"]}`, `{"args":"notarray"}`, `{"command":"x","env":"str"}`} {
		p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", meta))
		if err != nil {
			t.Errorf("meta %s: err = %v, want in-band", meta, err)
			continue
		}
		if gjson.Get(p.Raw, "workspace.error.code").String() != "bad_request" {
			t.Errorf("meta %s: payload = %s, want workspace.error.code bad_request", meta, p.Raw)
		}
	}
}

func TestExecWorkspaceSuccessShape(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0, Stdout: []byte("hello\n"), Stderr: []byte("warn"), StdoutTruncated: true, WallMS: 7}}
	pu, ctx := newWorkspaceUnit(t, stub)

	p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"echo hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Type != event.JSON {
		t.Fatalf("payload type = %v", p.Type)
	}
	checks := map[string]any{
		"_workspace.exit":             float64(0),
		"_workspace.stdout":           "hello\n",
		"_workspace.stderr":           "warn",
		"_workspace.stdout_truncated": true,
		"_workspace.stderr_truncated": false,
		"_txc.workspace.provider":     "stub",
		"_txc.workspace.computer":     "ref-tools",
		"_txc.workspace.exit":         float64(0),
		"_txc.workspace.duration_ms":  float64(7),
	}
	for path, want := range checks {
		if got := gjson.Get(p.Raw, path).Value(); got != want {
			t.Errorf("%s = %v (%T), want %v; raw=%s", path, got, got, want, p.Raw)
		}
	}
	if gjson.Get(p.Raw, "_txc.workspace.run").String() == "" {
		t.Errorf("no run id stamped: %s", p.Raw)
	}
	if gjson.Get(p.Raw, "workspace.error").Exists() {
		t.Errorf("success carries workspace.error: %s", p.Raw)
	}

	// Custom into.
	p, err = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"echo hello","into":"_ws"}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(p.Raw, "_ws.stdout").String() != "hello\n" || gjson.Get(p.Raw, "_workspace").Exists() {
		t.Errorf("custom into not honored: %s", p.Raw)
	}
}

func TestExecWorkspaceNonZeroExitIsData(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 3, Stderr: []byte("nope")}}
	pu, ctx := newWorkspaceUnit(t, stub)
	p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"exit 3"}`))
	if err != nil {
		t.Fatalf("non-zero exit surfaced as error: %v", err)
	}
	if gjson.Get(p.Raw, "_workspace.exit").Int() != 3 || gjson.Get(p.Raw, "workspace.error").Exists() {
		t.Errorf("payload = %s, want exit 3 and no workspace.error", p.Raw)
	}
}

func TestExecWorkspaceProviderFailureIsInBand(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{errors.New("boom"), "provider"},
		{workspace.ErrTimeout, "timeout"},
		{workspace.ErrNotAllowed, "not_allowed"},
		{&workspace.Error{Code: "capacity", Message: "429"}, "capacity"},
	}
	for _, c := range cases {
		stub := &stubProvider{err: c.err}
		pu, ctx := newWorkspaceUnit(t, stub)
		p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"x","into":"_ws"}`))
		if err != nil {
			t.Errorf("%v: surfaced as error %v, want in-band", c.err, err)
			continue
		}
		if got := gjson.Get(p.Raw, "workspace.error.code").String(); got != c.code {
			t.Errorf("%v: code = %q, want %q (raw %s)", c.err, got, c.code, p.Raw)
		}
		if !strings.Contains(gjson.Get(p.Raw, "workspace.error.message").String(), c.err.Error()) {
			t.Errorf("%v: message lost: %s", c.err, p.Raw)
		}
		if !gjson.Get(p.Raw, "_ws").IsObject() {
			t.Errorf("%v: <into> not present as {}: %s", c.err, p.Raw)
		}
	}
}

func TestExecWorkspaceVerbs(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, ctx := newWorkspaceUnit(t, stub)
	p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/destroy", `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if gjson.Get(p.Raw, "_workspace.state").String() != "destroyed" || stub.destroys.Load() != 1 {
		t.Errorf("destroy: %s (destroys=%d)", p.Raw, stub.destroys.Load())
	}
	p, err = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/sleep", `{}`))
	if err != nil || gjson.Get(p.Raw, "_workspace.state").String() != "sleeping" {
		t.Errorf("sleep: %s %v", p.Raw, err)
	}
	p, err = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/create", `{"into":"_ws"}`))
	if err != nil || gjson.Get(p.Raw, "_ws.state").String() != "created" || gjson.Get(p.Raw, "_txc.workspace.computer").String() != "ref-tools" {
		t.Errorf("create: %s %v", p.Raw, err)
	}
	p, err = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/wake", `{}`))
	if err != nil || gjson.Get(p.Raw, "_workspace.state").String() != "running" || gjson.Get(p.Raw, "_txc.workspace.run").String() == "" {
		t.Errorf("wake: %s %v", p.Raw, err)
	}
	p, err = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/checkpoint", `{"comment":"after install"}`))
	if err != nil || gjson.Get(p.Raw, "_workspace.checkpoint_ref").String() != "cp-after install" || gjson.Get(p.Raw, "_workspace.state").String() != "checkpointed" {
		t.Errorf("checkpoint: %s %v", p.Raw, err)
	}
	// exec + WITH checkpoint = true snapshots after a successful exec.
	p, err = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"make","checkpoint":true,"comment":"built"}`))
	if err != nil || gjson.Get(p.Raw, "_workspace.checkpoint_ref").String() != "cp-built" || gjson.Get(p.Raw, "_workspace.exit").Int() != 0 {
		t.Errorf("exec+checkpoint: %s %v", p.Raw, err)
	}
	// …but not after a failing one.
	stub.res = workspace.ExecResult{Exit: 2}
	p, _ = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"make","checkpoint":true}`))
	if gjson.Get(p.Raw, "_workspace.checkpoint_ref").Exists() {
		t.Errorf("checkpoint taken after a non-zero exit: %s", p.Raw)
	}
	// A provider without the capability reports it in-band.
	plain := struct{ workspace.Provider }{&stubProvider{res: workspace.ExecResult{Exit: 0}}}
	pu.Workspaces = workspace.NewManager(plain, workspace.Limits{}, nil)
	p, err = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/checkpoint", `{}`))
	if err != nil || gjson.Get(p.Raw, "workspace.error.code").String() != "unsupported" {
		t.Errorf("checkpoint on a plain provider: %s %v", p.Raw, err)
	}
	p, _ = pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"make","checkpoint":true}`))
	if gjson.Get(p.Raw, "_workspace.checkpoint_error.code").String() != "unsupported" || gjson.Get(p.Raw, "_workspace.exit").Int() != 0 {
		t.Errorf("exec+checkpoint on a plain provider: %s", p.Raw)
	}
}

func TestExecWorkspaceEmitsUsage(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0, Stdout: []byte("12345"), Stderr: []byte("67"), WallMS: 3}}
	pu, ctx := newWorkspaceUnit(t, stub)
	sink := &stubUsage{}
	pu.Usage = sink
	if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"x","stdin":"abc"}`)); err != nil {
		t.Fatal(err)
	}
	if sink.ev == nil {
		t.Fatal("no usage event")
	}
	ev := *sink.ev
	if ev.Src != "workspace" || ev.Tenant != "acme" || ev.Stack != "site/100/ws" || ev.Status != "ok" || ev.BytesIn != 3 || ev.BytesOut != 7 || ev.DurationMS != 3 {
		t.Errorf("usage event = %+v", ev)
	}
	stub.err = errors.New("down")
	if _, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if sink.ev.Status != "error" {
		t.Errorf("failure usage status = %q", sink.ev.Status)
	}
}

func TestWorkspaceSanitizeKeepsStampForWorkspaceTransportOnly(t *testing.T) {
	raw := `{"_ws":{"exit":0},"_txc":{"workspace":{"run":"r1","provider":"stub"},"tenant":"forged","web":{"res":{"status":200}}}}`
	ws := sanitizeAuthorOutputFor("workspace", raw)
	if gjson.Get(ws, "_txc.workspace.run").String() != "r1" {
		t.Errorf("workspace transport lost its stamp: %s", ws)
	}
	if gjson.Get(ws, "_txc.tenant").Exists() {
		t.Errorf("workspace transport may forge tenant: %s", ws)
	}
	if gjson.Get(ws, "_txc.web.res.status").Int() != 200 {
		t.Errorf("author-writable path dropped: %s", ws)
	}
	http := sanitizeAuthorOutputFor("http", raw)
	if gjson.Get(http, "_txc.workspace").Exists() {
		t.Errorf("http transport may forge _txc.workspace: %s", http)
	}
	if !transportAuthorControlled("workspace") {
		t.Error("workspace transport must be author-controlled (sanitized)")
	}
}

// seedWorkspaceOp seeds a rule owned by tenant slug "acme" (and the tenant
// row itself) so a Run pinned to that tenant, as every real request is,
// finds it.
func seedWorkspaceOp(t *testing.T, pu *Unit, stack string, scope int, name, rule string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(`INSERT OR IGNORE INTO tenants (tenant_id, slug) VALUES ('tnt_acme', 'acme')`); err != nil {
		t.Fatal(err)
	}
	seedTenantOp(t, pu, stack, scope, name, rule)
}

func runTenanted(t *testing.T, pu *Unit, stage string) string {
	t.Helper()
	resCh := make(chan event.Payload, 4)
	done := make(chan error, 1)
	go func() { done <- pu.Run(context.Background(), `{"_txc":{"tenant":"acme"}}`, stage, resCh) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s")
	}
	select {
	case p := <-resCh:
		return p.Raw
	default:
		t.Fatal("Run emitted no payload")
	}
	return ""
}

// TestWorkspaceDispatchMergesAndGoto is the dispatch-parity proof (cf.
// TestComputeDispatchMergesAndGoto): the workspace output merges into the
// envelope under `into`, the chassis stamp survives the untrusted-output
// sanitizer, an EMIT on the same rule drives a `_txc.goto` through the
// ordinary machinery, and the wall-clock fuel is charged.
func TestWorkspaceDispatchMergesAndGoto(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0, Stdout: []byte("hi\n"), WallMS: 1500}}
	pu, _ := newWorkspaceUnit(t, stub)
	pu.Conf.WorkspaceDefaultTimeout = "60s"

	seedWorkspaceOp(t, pu, "site", 100, "ws", `EXEC "workspace://tools/exec" WITH command = "echo hi", into = "_ws" EMIT @goto = "site/200"`)
	seedWorkspaceOp(t, pu, "site", 200, "render", `EMIT .rendered = true`)

	out := runTenanted(t, pu, "site/100")
	if gjson.Get(out, "_ws.stdout").String() != "hi\n" || gjson.Get(out, "_ws.exit").Int() != 0 {
		t.Fatalf("workspace output not merged: %s", out)
	}
	if !gjson.Get(out, "rendered").Bool() {
		t.Fatalf("EMIT @goto on the workspace rule did not drive the jump to site/200: %s", out)
	}
	if gjson.Get(out, "_txc.workspace.run").String() == "" || gjson.Get(out, "_txc.workspace.provider").String() != "stub" {
		t.Errorf("chassis stamp did not survive the sanitizer: %s", out)
	}
	if fuel := FuelUsedFromEnvelope(out); fuel < 36 {
		t.Errorf("fuel_used = %d, want >= 36 (scope 10 + exec 25 + 1.5 s → one 30 s period = 1): %s", fuel, out)
	}
	stub.mu.Lock()
	spec := stub.seenSpec
	stub.mu.Unlock()
	if spec.Tenant != "acme" || spec.Stack != "site" || spec.Name != "tools" {
		t.Errorf("spec = %+v", spec)
	}
}

// TestWorkspaceDefaultTimeout: a workspace:// op with no WITH timeout gets
// --workspace-default-timeout, not the 1s general default.
func TestWorkspaceDefaultTimeout(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, _ := newWorkspaceUnit(t, stub)
	pu.Conf.OpTimeout = "1s"
	pu.Conf.WorkspaceDefaultTimeout = "7s"
	seedWorkspaceOp(t, pu, "site", 100, "ws", `EXEC "workspace://tools/exec" WITH command = "true"`)
	before := time.Now()
	runTenanted(t, pu, "site/100")
	stub.mu.Lock()
	d := stub.deadline
	stub.mu.Unlock()
	if d.IsZero() {
		t.Fatal("no deadline on the op ctx")
	}
	if got := d.Sub(before); got < 5*time.Second || got > 8*time.Second {
		t.Errorf("deadline %v from dispatch, want ≈7s (workspace default), not the 1s op-timeout", got)
	}
}

// TestLoopWorkspaceAdmitted: workspace:// may LOOP — three passes inside one
// dispatch, bookkeeping written.
func TestLoopWorkspaceAdmitted(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0, Stdout: []byte("x")}}
	pu, _ := newWorkspaceUnit(t, stub)
	pu.Conf.LoopTimeout = "60s"
	pu.Conf.WorkspaceDefaultTimeout = "60s"
	pu.Conf.OpLoopMax = 5
	seedWorkspaceOp(t, pu, "site", 100, "ws", `EXEC "workspace://tools/exec" WITH command = "true", into = "_ws" LOOP EVERY "1ms" UNTIL ._ws.never == true MAX 3`)
	out := runTenanted(t, pu, "site/100")
	if got := stub.execs.Load(); got != 3 {
		t.Errorf("execs = %d, want 3", got)
	}
	if gjson.Get(out, "_txc.runtime.loop.ws.passes").Int() != 3 || gjson.Get(out, "_txc.runtime.loop.ws.stop").String() != "max" {
		t.Errorf("loop bookkeeping: %s", out)
	}
	if !loopTransportAdmitted("workspace://tools/exec") {
		t.Error("loopTransportAdmitted(workspace://) = false")
	}
}

// TestExecWorkspaceSecretsEnvMaterializedAndScrubbed: a
// `secrets.env.<NAME>` ref lands in the command's environment (formatted),
// and its cleartext never reaches the payload — stdout, stderr and a
// provider error message are all scrubbed.
func TestExecWorkspaceSecretsEnvMaterializedAndScrubbed(t *testing.T) {
	const cleartext = "tok-s3cr3t-value-0123456789"
	stub := &stubProvider{echoEnv: true}
	pu, ctx := newWorkspaceUnit(t, stub)
	op := workspaceOp("workspace://tools/exec",
		`{"command":"env","secrets":{"env":{"TOKEN":{"secret":"PONY_TOKEN","format":"Bearer {}"},"PLAIN":{"secret":"PONY_TOKEN"}}}}`)
	op.Secrets.Set("PONY_TOKEN", []byte(cleartext))

	p, err := pu.ExecWorkspace(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	req := stub.seen
	stub.mu.Unlock()
	if req.Env["TOKEN"] != "Bearer "+cleartext || req.Env["PLAIN"] != cleartext {
		t.Errorf("env not materialized: %+v", req.Env)
	}
	if strings.Contains(p.Raw, cleartext) {
		t.Fatalf("cleartext leaked into the payload: %s", p.Raw)
	}
	if out := gjson.Get(p.Raw, "_workspace.stdout").String(); !strings.Contains(out, "TOKEN=Bearer [REDACTED]") || !strings.Contains(out, "PLAIN=[REDACTED]") {
		t.Errorf("stdout not scrubbed as expected: %q", out)
	}
	if errOut := gjson.Get(p.Raw, "_workspace.stderr").String(); errOut != "err:[REDACTED]" {
		t.Errorf("stderr not scrubbed: %q", errOut)
	}

	// A provider failure whose message echoes the value is scrubbed too.
	stub.err = errors.New("dial failed for " + cleartext)
	p, _ = pu.ExecWorkspace(ctx, op)
	if strings.Contains(p.Raw, cleartext) || !strings.Contains(gjson.Get(p.Raw, "workspace.error.message").String(), "[REDACTED]") {
		t.Errorf("error message not scrubbed: %s", p.Raw)
	}

	// An optional secret the store could not supply is simply absent.
	stub.err = nil
	op = workspaceOp("workspace://tools/exec",
		`{"command":"env","secrets":{"env":{"MAYBE":{"secret":"NOPE","optional":true}}}}`)
	p, err = pu.ExecWorkspace(ctx, op)
	if err != nil || gjson.Get(p.Raw, "workspace.error").Exists() {
		t.Errorf("optional absent secret: %s %v", p.Raw, err)
	}
	stub.mu.Lock()
	req = stub.seen
	stub.mu.Unlock()
	if _, present := req.Env["MAYBE"]; present {
		t.Errorf("optional absent secret set in env: %+v", req.Env)
	}
}

func TestScrubSecretsShortValuesLeftAlone(t *testing.T) {
	var bag secrets.SecretBag
	bag.Set("SHORT", []byte("abc"))
	bag.Set("LONG", []byte("longer-secret-value"))
	out := scrubSecrets([]byte("abc longer-secret-value abc"), bag)
	if string(out) != "abc [REDACTED] abc" {
		t.Errorf("scrub = %q", out)
	}
	if got := scrubSecrets(nil, bag); got != nil {
		t.Errorf("scrub(nil) = %q", got)
	}
}

// TestWorkspaceLoopKeepsLongerDefault: a looping workspace exec is bounded
// by the longer of --loop-timeout and --workspace-default-timeout, so a
// loop of tool runs is never cut shorter than one tool run would be.
func TestWorkspaceLoopKeepsLongerDefault(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, _ := newWorkspaceUnit(t, stub)
	pu.Conf.OpTimeout = "1s"
	pu.Conf.LoopTimeout = "3s"
	pu.Conf.WorkspaceDefaultTimeout = "8s"
	pu.Conf.OpLoopMax = 5
	seedWorkspaceOp(t, pu, "site", 100, "ws", `EXEC "workspace://tools/exec" WITH command = "true", into = "_ws" LOOP EVERY "1ms" UNTIL ._ws.never == true MAX 1`)
	before := time.Now()
	runTenanted(t, pu, "site/100")
	stub.mu.Lock()
	d := stub.deadline
	stub.mu.Unlock()
	if got := d.Sub(before); got < 6*time.Second || got > 9*time.Second {
		t.Errorf("loop deadline %v from dispatch, want ≈8s (the longer workspace default), not the 3s loop default", got)
	}
}

func TestWorkspaceFuelRate(t *testing.T) {
	for wall, want := range map[int64]int64{
		0:       1, // minimum: every accounted op pays at least 1
		1:       1,
		29_999:  1,
		30_000:  1,
		30_001:  2,
		60_000:  2, // 2 per minute
		300_000: 10,
		301_000: 11,
	} {
		if got := workspaceFuel(wall); got != want {
			t.Errorf("workspaceFuel(%d ms) = %d, want %d", wall, got, want)
		}
	}
}
