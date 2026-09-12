package processor

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// stubSession is a canned workspace.ExecSession: fixed stdout, fixed exit,
// records resizes and signals.
type stubSession struct {
	stdout  io.Reader
	exit    int
	mu      sync.Mutex
	resized [][2]uint16
	signals []string
	closed  atomic.Bool
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (s *stubSession) Stdin() io.WriteCloser { return nopWriteCloser{io.Discard} }
func (s *stubSession) Stdout() io.Reader     { return s.stdout }
func (s *stubSession) Resize(c, r uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resized = append(s.resized, [2]uint16{c, r})
	return nil
}
func (s *stubSession) Signal(sig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signals = append(s.signals, sig)
	return nil
}
func (s *stubSession) Wait() (workspace.ExecResult, error) {
	return workspace.ExecResult{Exit: s.exit, WallMS: 5}, nil
}
func (s *stubSession) Close() error { s.closed.Store(true); return nil }

// startableStub is a stubProvider whose woken computer also implements
// workspace.Starter, so `tty = true` has somewhere to go.
type startableStub struct {
	*stubProvider
	sess *stubSession

	mu      sync.Mutex
	started workspace.ExecRequest
	starts  int
}

func (s *startableStub) Wake(context.Context, workspace.Handle) (workspace.Computer, error) {
	return s, nil
}

func (s *startableStub) Start(_ context.Context, req workspace.ExecRequest, _ workspace.Limits) (workspace.ExecSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = req
	s.starts++
	return s.sess, nil
}

// TestExecWorkspaceTTYUnsupported: a provider with no session capability
// answers `tty = true` in-band as unsupported, never a Go error.
func TestExecWorkspaceTTYUnsupported(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, ctx := newWorkspaceUnit(t, stub)
	p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"vi","tty":true}`))
	if err != nil {
		t.Fatalf("ExecWorkspace: %v", err)
	}
	if got := gjson.Get(p.Raw, "workspace.error.code").String(); got != "unsupported" {
		t.Fatalf("error code = %q, want unsupported; payload %s", got, p.Raw)
	}
	if stub.execs.Load() != 0 {
		t.Error("the one-shot Exec must not run for a tty request")
	}
}

// TestExecWorkspaceTTYRunsSession: `tty = true` goes through Start, the
// session's output lands as stdout, geometry rides the request, stderr is
// empty, and the session is closed afterwards.
func TestExecWorkspaceTTYRunsSession(t *testing.T) {
	sess := &stubSession{stdout: strings.NewReader("\x1b[1mterm-out\x1b[0m"), exit: 3}
	ss := &startableStub{stubProvider: &stubProvider{}, sess: sess}
	pu, _ := newTestUnit(t)
	pu.Workspaces = workspace.NewManager(ss, workspace.Limits{}, nil)
	ctx := WithTenant(context.Background(), "acme")

	p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec",
		`{"command":"top","tty":true,"cols":100,"rows":30,"into":"_t"}`))
	if err != nil {
		t.Fatalf("ExecWorkspace: %v", err)
	}
	if code := gjson.Get(p.Raw, "workspace.error.code").String(); code != "" {
		t.Fatalf("unexpected error %s", p.Raw)
	}
	if got := gjson.Get(p.Raw, "_t.stdout").String(); got != "\x1b[1mterm-out\x1b[0m" {
		t.Errorf("stdout = %q", got)
	}
	if got := gjson.Get(p.Raw, "_t.exit").Int(); got != 3 {
		t.Errorf("exit = %d, want 3 (exit codes are data)", got)
	}
	if got := gjson.Get(p.Raw, "_t.stderr").String(); got != "" {
		t.Errorf("stderr = %q, want empty on a tty", got)
	}
	ss.mu.Lock()
	req := ss.started
	ss.mu.Unlock()
	if !req.TTY || req.Cols != 100 || req.Rows != 30 || req.Command != "top" {
		t.Errorf("Start saw %+v", req)
	}
	if !sess.closed.Load() {
		t.Error("session not closed after the exec")
	}
	if ss.stubProvider.execs.Load() != 0 {
		t.Error("one-shot Exec ran alongside the session")
	}
}

// TestWorkspaceRequestTTYShape: the WITH keys are typed and cols/rows need tty.
func TestWorkspaceRequestTTYShape(t *testing.T) {
	cases := map[string]string{
		`{"command":"x","tty":"yes"}`:         "tty must be a boolean",
		`{"command":"x","cols":80}`:           "needs tty = true",
		`{"command":"x","tty":true,"rows":0}`: "between 1 and 65535",
	}
	for meta, want := range cases {
		_, err := workspaceRequest(workspaceOp("workspace://tools/exec", meta))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", meta, err, want)
		}
	}
	req, err := workspaceRequest(workspaceOp("workspace://tools/exec", `{"command":"x","tty":true}`))
	if err != nil || !req.TTY || req.Cols != 0 || req.Rows != 0 {
		t.Errorf("bare tty: %+v, %v", req, err)
	}
	if c, r := req.Geometry(); c != 80 || r != 24 {
		t.Errorf("default geometry = %dx%d, want 80x24", c, r)
	}
}
