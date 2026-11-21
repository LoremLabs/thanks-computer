package processor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// gatedProvider serves one tenant and can detach, as the owner provider on
// an attached machine does.
type gatedProvider struct {
	stubProvider
	only string

	mu       sync.Mutex
	detached []workspace.ExecRequest
}

func (g *gatedProvider) ServesTenant(slug string) bool { return slug == g.only }
func (g *gatedProvider) Wake(context.Context, workspace.Handle) (workspace.Computer, error) {
	return g, nil
}
func (g *gatedProvider) ExecDetached(_ context.Context, req workspace.ExecRequest, _ workspace.Limits) (workspace.DetachedJob, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.detached = append(g.detached, req)
	return workspace.DetachedJob{ID: "job_0123456789abcdef0123456789abcdef", StartedAt: time.Unix(1_700_000_000, 0)}, nil
}

// A provider that serves one tenant runs nothing for any other, whatever the
// verb, and the tenant asked about is the request's pinned one.
func TestExecWorkspaceTenantGate(t *testing.T) {
	g := &gatedProvider{only: "_attach", stubProvider: stubProvider{res: workspace.ExecResult{Exit: 0, Stdout: []byte("ran")}}}
	pu, _ := newTestUnit(t)
	pu.Workspaces = workspace.NewManager(g, workspace.Limits{}, nil)

	for _, tenant := range []string{"acme", "_sys", "default"} {
		ctx := WithTenant(context.Background(), tenant)
		for _, ref := range []string{"workspace://tools/exec", "workspace://tools/wake", "workspace://tools/destroy"} {
			// An envelope that CLAIMS the served tenant changes nothing.
			op := workspaceOp(ref, `{"command":"true"}`)
			op.Input = `{"_txc":{"tenant":"_attach"}}`
			pl, err := pu.ExecWorkspace(ctx, op)
			if err != nil {
				t.Fatalf("%s as %s: %v", ref, tenant, err)
			}
			if got := gjson.Get(pl.Raw, "workspace.error.code").String(); got != "not_allowed" {
				t.Errorf("%s as %s: workspace.error.code = %q, want not_allowed (%s)", ref, tenant, got, pl.Raw)
			}
		}
	}
	if n := g.execs.Load(); n != 0 {
		t.Fatalf("a refused tenant's command ran %d times", n)
	}

	pl, err := pu.ExecWorkspace(WithTenant(context.Background(), "_attach"), workspaceOp("workspace://tools/exec", `{"command":"true"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.Get(pl.Raw, "_workspace.stdout").String(); got != "ran" {
		t.Fatalf("the served tenant's exec = %s", pl.Raw)
	}
}

// `WITH detach = true` answers the job at once. WITH timeout is the job's
// life, handed to the provider as such, and nothing waits on it here.
func TestExecWorkspaceDetach(t *testing.T) {
	g := &gatedProvider{only: "acme"}
	pu, _ := newTestUnit(t)
	pu.Conf.OpTimeoutMax = "24h"
	pu.Workspaces = workspace.NewManager(g, workspace.Limits{}, nil)
	ctx := WithTenant(context.Background(), "acme")

	pl, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec",
		`{"command":"make all","detach":true,"timeout":"2h","stdin":"in","cwd":"sub","into":"job"}`))
	if err != nil {
		t.Fatal(err)
	}
	if e := gjson.Get(pl.Raw, "workspace.error"); e.Exists() {
		t.Fatalf("detach failed: %s", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "job.job").String(); got != "job_0123456789abcdef0123456789abcdef" {
		t.Fatalf("job = %s", pl.Raw)
	}
	if !gjson.Get(pl.Raw, "job.detached").Bool() || gjson.Get(pl.Raw, "job.exit").Exists() {
		t.Fatalf("a detached exec's answer = %s: it has no exit yet", pl.Raw)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.detached) != 1 || g.execs.Load() != 0 {
		t.Fatalf("detached %d, held %d", len(g.detached), g.execs.Load())
	}
	req := g.detached[0]
	if req.Command != "make all" || string(req.Stdin) != "in" || req.Cwd != "sub" || req.Timeout != 2*time.Hour {
		t.Fatalf("request = %+v", req)
	}

	for name, meta := range map[string]string{
		"with stream":       `{"command":"x","detach":true,"stream":true}`,
		"with a terminal":   `{"command":"x","detach":true,"tty":true}`,
		"detach not a bool": `{"command":"x","detach":"yes"}`,
	} {
		pl, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", meta))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := gjson.Get(pl.Raw, "workspace.error.code").String(); got != "bad_request" {
			t.Errorf("%s: %s", name, pl.Raw)
		}
	}
}

// A provider that cannot detach says so; nothing is quietly run held.
func TestExecWorkspaceDetachUnsupported(t *testing.T) {
	stub := &stubProvider{res: workspace.ExecResult{Exit: 0}}
	pu, ctx := newWorkspaceUnit(t, stub)
	pl, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://tools/exec", `{"command":"x","detach":true,"timeout":"1m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.Get(pl.Raw, "workspace.error.code").String(); got != "unsupported" {
		t.Fatalf("answer = %s", pl.Raw)
	}
	if stub.execs.Load() != 0 {
		t.Fatal("the command was run held instead")
	}
}
