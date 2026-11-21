package processor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// asyncProvider is a workspace whose computer takes async execs, as an
// attached machine's does. answer says what the handoff returns.
type asyncProvider struct {
	stubProvider
	answer string // "started" | "result" | "away" | "unknown"

	mu       sync.Mutex
	ticket   workspace.AsyncTicket
	req      workspace.ExecRequest
	handoffs int
	handoff  time.Duration // how long the handoff was given
}

func (a *asyncProvider) Name() string { return "async-stub" }
func (a *asyncProvider) Create(_ context.Context, spec workspace.Spec) (workspace.Handle, error) {
	return workspace.Handle{Provider: "async-stub", Ref: "atc-cred", Name: spec.Name}, nil
}
func (a *asyncProvider) Wake(context.Context, workspace.Handle) (workspace.Computer, error) {
	return a, nil
}
func (a *asyncProvider) ExecAsync(ctx context.Context, req workspace.ExecRequest, _ workspace.Limits, t workspace.AsyncTicket) (string, workspace.ExecResult, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handoffs++
	a.ticket, a.req = t, req
	if d, ok := ctx.Deadline(); ok {
		a.handoff = time.Until(d)
	}
	switch a.answer {
	case "started":
		return "job_0123456789abcdef0123456789abcdef", workspace.ExecResult{}, true, nil
	case "result":
		return "", workspace.ExecResult{Exit: 2, Stdout: []byte("ran in place")}, false, nil
	case "away":
		return "", workspace.ExecResult{Exit: -1}, false, &workspace.Error{Code: workspace.CodeAway, Message: "the machine is not connected"}
	default:
		return "", workspace.ExecResult{Exit: -1}, false, &workspace.Error{Code: workspace.CodeUnknown, Message: "no reply"}
	}
}

func (a *asyncProvider) got() (workspace.AsyncTicket, workspace.ExecRequest, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ticket, a.req, a.handoff
}

var asyncWID = workspace.ID("acme", "site", "tools")

const asyncRule = `EXEC "workspace://tools/exec" WITH command = "make all", mode = "async", max_duration = "1h", into = "build"`

// asyncUnit seeds one async workspace op at site/100 and a later scope that
// shows the stage went on.
func asyncUnit(t *testing.T, a *asyncProvider, rule string) *Unit {
	t.Helper()
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pu.Runs = continuation.NewRuns(fs)
	pu.Workspaces = workspace.NewManager(a, workspace.Limits{}, nil)
	seedWorkspaceOp(t, pu, "site", 100, "build", rule)
	seedWorkspaceOp(t, pu, "site", 200, "after", `EMIT .went_on = true`)
	return pu
}

// start runs site/100 and returns the suspended run's id.
func startAsync(t *testing.T, pu *Unit) string {
	t.Helper()
	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"tenant":"acme"}}`, "site/100", resCh) }()
	rcid, _ := waitFor202(t, resCh)
	return resolveRunIDFromRcid(t, pu, rcid)
}

func runResult(t *testing.T, pu *Unit, runID string) []byte {
	t.Helper()
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}
	res, ok, _ := pu.Runs.ReadResult(context.Background(), runID)
	if !ok {
		t.Fatal("no result")
	}
	return res
}

func stillWaiting(t *testing.T, pu *Unit, runID string) {
	t.Helper()
	time.Sleep(150 * time.Millisecond)
	st, _ := pu.Runs.RunState(context.Background(), runID)
	if st == continuation.StateCompleted || st == continuation.StateFailed {
		t.Fatalf("the run is %s; it should still be waiting for its machine", st)
	}
}

func codeOf(err error) string {
	var we *workspace.Error
	if errors.As(err, &we) {
		return we.Code
	}
	if err != nil {
		return "!" + err.Error()
	}
	return ""
}

// The whole path: the op suspends the run, the machine is handed the
// command with the op's continuation, and the result that later comes back
// for it — from the right machine, with the right token — resumes the run.
func TestWorkspaceAsyncStartedThenCompleted(t *testing.T) {
	a := &asyncProvider{answer: "started"}
	pu := asyncUnit(t, a, asyncRule)
	runID := startAsync(t, pu)
	ctx := context.Background()

	ticket, req, handoff := a.got()
	if ticket.OPC == "" || ticket.Token == "" {
		t.Fatalf("the machine was not handed a continuation: %+v", ticket)
	}
	// The command's life is max_duration, less the margin the machine is
	// given to report a timeout before the run expires.
	if req.Command != "make all" || req.Timeout != time.Hour-workspaceAsyncMargin {
		t.Fatalf("request = %+v", req)
	}
	if handoff <= 0 || handoff > workspaceAsyncHandoff {
		t.Fatalf("the handoff was given %s", handoff)
	}
	stillWaiting(t, pu, runID)

	done := workspace.AsyncResult{Result: workspace.ExecResult{Exit: 7, Stdout: []byte("built\n"), Stderr: []byte("warn\n"), WallMS: 20}}

	// Refusals, none of which records anything.
	for name, c := range map[string]struct {
		tenant, wid, opc, token, want string
	}{
		"a wrong token":      {"acme", asyncWID, ticket.OPC, "not-the-token", "not_allowed"},
		"no token":           {"acme", asyncWID, ticket.OPC, "", "not_allowed"},
		"another workspace":  {"acme", workspace.ID("acme", "site", "other"), ticket.OPC, ticket.Token, "not_allowed"},
		"another tenant":     {"evil", asyncWID, ticket.OPC, ticket.Token, "not_allowed"},
		"an unknown opc":     {"acme", asyncWID, "opc_nope", ticket.Token, workspace.CodeNotFound},
		"another op's token": {"acme", asyncWID, "opc_nope", "x", workspace.CodeNotFound},
	} {
		err := pu.CompleteWorkspaceAsync(ctx, c.tenant, c.wid, "atc-cred", c.opc, c.token, done)
		if codeOf(err) != c.want {
			t.Errorf("%s: %v, want %s", name, err, c.want)
		}
	}
	stillWaiting(t, pu, runID)

	if err := pu.CompleteWorkspaceAsync(ctx, "acme", asyncWID, "atc-cred", ticket.OPC, ticket.Token, done); err != nil {
		t.Fatalf("the machine's own completion: %v", err)
	}
	res := runResult(t, pu, runID)
	if gjson.GetBytes(res, "build.exit").Int() != 7 || gjson.GetBytes(res, "build.stdout").String() != "built\n" ||
		gjson.GetBytes(res, "build.stderr").String() != "warn\n" {
		t.Fatalf("result = %s", res)
	}
	if !gjson.GetBytes(res, "went_on").Bool() {
		t.Fatalf("the later scope did not run: %s", res)
	}
	// The chassis's own stamp survived the merge, as a held exec's does.
	if gjson.GetBytes(res, "_txc.workspace.provider").String() != workspace.ProviderAttached ||
		gjson.GetBytes(res, "_txc.workspace.computer").String() != "atc-cred" ||
		gjson.GetBytes(res, "_txc.workspace.exit").Int() != 7 {
		t.Fatalf("stamp = %s", gjson.GetBytes(res, "_txc.workspace").Raw)
	}

	// The same completion again, and a different one: acknowledged, and
	// nothing changes. The first terminal stands.
	late := workspace.AsyncResult{Result: workspace.ExecResult{Exit: 0, Stdout: []byte("a second story")}}
	for _, r := range []workspace.AsyncResult{done, late} {
		if err := pu.CompleteWorkspaceAsync(ctx, "acme", asyncWID, "atc-cred", ticket.OPC, ticket.Token, r); err != nil {
			t.Fatalf("a repeated completion: %v", err)
		}
	}
	again, _, _ := pu.Runs.ReadResult(ctx, runID)
	if string(again) != string(res) {
		t.Fatalf("a late completion changed the run's result:\n%s\n%s", res, again)
	}
}

// What a machine supplies is two strings and a number. Whatever it puts in
// them stays under `into`: it cannot write elsewhere in the envelope or
// forge the chassis's stamp.
func TestWorkspaceAsyncResultIsConfined(t *testing.T) {
	a := &asyncProvider{answer: "started"}
	pu := asyncUnit(t, a, asyncRule)
	runID := startAsync(t, pu)
	ticket, _, _ := a.got()
	hostile := workspace.AsyncResult{Result: workspace.ExecResult{
		Stdout: []byte(`{"went_on":"forged","_txc":{"workspace":{"provider":"evil"},"tenant":"other"},"build":{"exit":0}}`),
	}}
	if err := pu.CompleteWorkspaceAsync(context.Background(), "acme", asyncWID, "atc-cred", ticket.OPC, ticket.Token, hostile); err != nil {
		t.Fatal(err)
	}
	res := runResult(t, pu, runID)
	if gjson.GetBytes(res, "build.stdout").String() != string(hostile.Result.Stdout) {
		t.Fatalf("stdout was not kept as a string: %s", res)
	}
	if gjson.GetBytes(res, "_txc.workspace.provider").String() != workspace.ProviderAttached ||
		gjson.GetBytes(res, "_txc.tenant").String() != "acme" ||
		gjson.GetBytes(res, "went_on").Raw != "true" {
		t.Fatalf("the machine's output reached outside `into`: %s", res)
	}
}

// A failed or timed-out command is data for the stage to read: the terminal
// is completed, with workspace.error inside, and the later scope runs.
func TestWorkspaceAsyncFailureIsData(t *testing.T) {
	a := &asyncProvider{answer: "started"}
	pu := asyncUnit(t, a, asyncRule)
	runID := startAsync(t, pu)
	ticket, _, _ := a.got()
	timedOut := workspace.AsyncResult{ErrCode: "timeout", ErrMessage: "the command ran out of time"}
	if err := pu.CompleteWorkspaceAsync(context.Background(), "acme", asyncWID, "atc-cred", ticket.OPC, ticket.Token, timedOut); err != nil {
		t.Fatal(err)
	}
	res := runResult(t, pu, runID)
	if gjson.GetBytes(res, "workspace.error.code").String() != "timeout" || !gjson.GetBytes(res, "went_on").Bool() {
		t.Fatalf("result = %s", res)
	}

	// A code that is not a code is not passed through as one.
	b := &asyncProvider{answer: "started"}
	pu2 := asyncUnit(t, b, asyncRule)
	run2 := startAsync(t, pu2)
	t2, _, _ := b.got()
	odd := workspace.AsyncResult{ErrCode: `x","y":"z`, ErrMessage: "m"}
	if err := pu2.CompleteWorkspaceAsync(context.Background(), "acme", asyncWID, "atc-cred", t2.OPC, t2.Token, odd); err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(runResult(t, pu2, run2), "workspace.error.code").String(); got != "provider" {
		t.Fatalf("code = %q", got)
	}
}

// A command that could not be delivered settles at once: nothing will ever
// report for it, so the dispatch records `away` and resumes the run itself.
func TestWorkspaceAsyncAwayResumesAtOnce(t *testing.T) {
	a := &asyncProvider{answer: "away"}
	pu := asyncUnit(t, a, asyncRule)
	res := runResult(t, pu, startAsync(t, pu))
	if gjson.GetBytes(res, "workspace.error.code").String() != workspace.CodeAway || !gjson.GetBytes(res, "went_on").Bool() {
		t.Fatalf("result = %s", res)
	}
}

// The machine's own stack ran the command in place and answered with the
// result: that is the terminal, recorded at dispatch.
func TestWorkspaceAsyncResultInPlace(t *testing.T) {
	a := &asyncProvider{answer: "result"}
	pu := asyncUnit(t, a, asyncRule)
	res := runResult(t, pu, startAsync(t, pu))
	if gjson.GetBytes(res, "build.exit").Int() != 2 || gjson.GetBytes(res, "build.stdout").String() != "ran in place" {
		t.Fatalf("result = %s", res)
	}
}

// A handoff that was not confirmed records NOTHING. The command may be
// running; writing "unknown" now would throw away its real result, because
// the first terminal stands. The machine's report settles it.
func TestWorkspaceAsyncUnknownHandoffWaitsForTheMachine(t *testing.T) {
	a := &asyncProvider{answer: "unknown"}
	pu := asyncUnit(t, a, asyncRule)
	runID := startAsync(t, pu)
	stillWaiting(t, pu, runID)
	ticket, _, _ := a.got()
	done := workspace.AsyncResult{Result: workspace.ExecResult{Exit: 0, Stdout: []byte("it did run")}}
	if err := pu.CompleteWorkspaceAsync(context.Background(), "acme", asyncWID, "atc-cred", ticket.OPC, ticket.Token, done); err != nil {
		t.Fatal(err)
	}
	res := runResult(t, pu, runID)
	if gjson.GetBytes(res, "build.stdout").String() != "it did run" {
		t.Fatalf("the real result was lost: %s", res)
	}
}

// A terminal recorded by a holder that then died before claiming the
// resume: the machine sends again, and that second delivery resumes the run.
func TestWorkspaceAsyncSecondDeliveryResumes(t *testing.T) {
	a := &asyncProvider{answer: "started"}
	pu := asyncUnit(t, a, asyncRule)
	runID := startAsync(t, pu)
	ticket, _, _ := a.got()
	ctx := context.Background()
	lk, err := pu.Runs.ResolveOpContinuation(ctx, ticket.OPC)
	if err != nil {
		t.Fatal(err)
	}
	// The first holder got as far as the terminal, and no further.
	pl := workspaceSuccess("build", workspace.ProviderAttached, "atc-cred", "", workspace.ExecResult{Exit: 5}, 1, false)
	if recorded, err := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, "completed",
		continuation.TerminalMeta{Transport: "workspace"}, []byte(pl.Raw)); err != nil || !recorded {
		t.Fatalf("recorded %v, %v", recorded, err)
	}
	stillWaiting(t, pu, runID)
	if err := pu.CompleteWorkspaceAsync(ctx, "acme", asyncWID, "atc-cred", ticket.OPC, ticket.Token,
		workspace.AsyncResult{Result: workspace.ExecResult{Exit: 5}}); err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(runResult(t, pu, runID), "build.exit").Int(); got != 5 {
		t.Fatalf("exit = %d", got)
	}
}

func TestWorkspaceAsyncRefusals(t *testing.T) {
	for name, c := range map[string]struct{ rule, want string }{
		"no max_duration":            {`EXEC "workspace://tools/exec" WITH command = "x", mode = "async", into = "build"`, "bad_request"},
		"with detach":                {`EXEC "workspace://tools/exec" WITH command = "x", mode = "async", max_duration = "1h", detach = true, into = "build"`, "bad_request"},
		"with a terminal":            {`EXEC "workspace://tools/exec" WITH command = "x", mode = "async", max_duration = "1h", tty = true, into = "build"`, "bad_request"},
		"a timeout past the ceiling": {`EXEC "workspace://tools/exec" WITH command = "x", mode = "async", max_duration = "1h", timeout = "48h", into = "build"`, "bad_request"},
	} {
		a := &asyncProvider{answer: "started"}
		pu := asyncUnit(t, a, c.rule)
		res := runResult(t, pu, startAsync(t, pu))
		if got := gjson.GetBytes(res, "workspace.error.code").String(); got != c.want {
			t.Errorf("%s: %s", name, res)
		}
		if !gjson.GetBytes(res, "went_on").Bool() {
			t.Errorf("%s: the stage did not go on: %s", name, res)
		}
		a.mu.Lock()
		if a.handoffs != 0 {
			t.Errorf("%s: the command was handed over anyway", name)
		}
		a.mu.Unlock()
	}
}

// A workspace that cannot report a command's end itself says so; the
// command is not quietly run some other way.
func TestWorkspaceAsyncNeedsACapableWorkspace(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pu.Runs = continuation.NewRuns(fs)
	pu.Workspaces = workspace.NewManager(stub, workspace.Limits{}, nil)
	seedWorkspaceOp(t, pu, "site", 100, "build", asyncRule)
	seedWorkspaceOp(t, pu, "site", 200, "after", `EMIT .went_on = true`)
	res := runResult(t, pu, startAsync(t, pu))
	if got := gjson.GetBytes(res, "workspace.error.code").String(); got != "unsupported" {
		t.Fatalf("result = %s", res)
	}
	if stub.execs.Load() != 0 {
		t.Fatal("the command was run held instead")
	}
}

func TestIsWorkspaceAsyncOp(t *testing.T) {
	for exec, want := range map[string]bool{
		"workspace://tools/exec":         true,
		"workspace://computer/rain/exec": true,
		"workspace://tools/wake":         false,
		"workspace://tools/destroy":      false,
		"https://worker.example.com/job": false,
	} {
		op := opWith(exec, `{"mode":"async"}`)
		if got := isWorkspaceAsyncOp(op); got != want {
			t.Errorf("%s: isWorkspaceAsyncOp = %v, want %v", exec, got, want)
		}
	}
	if isAsyncOp(opWith("workspace://tools/wake", `{"mode":"async"}`)) {
		t.Error("a workspace verb other than exec was classified async")
	}
	if isAsyncOp(opWith("workspace://tools/exec", `{"mode":"continuable"}`)) || isAsyncOp(opWith("workspace://tools/exec", `{}`)) {
		t.Error("a workspace exec without mode = async was classified async")
	}
}
