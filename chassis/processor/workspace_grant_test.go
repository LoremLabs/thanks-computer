package processor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// reachingProvider is a provider whose commands run on the chassis's own
// machine, as the local one's do.
type reachingProvider struct{ *stubProvider }

func (reachingProvider) ReachesGrants() bool { return true }

// farProvider says it has the capability and that it does not apply.
type farProvider struct{ *stubProvider }

func (farProvider) ReachesGrants() bool { return false }

// stubHandoff stands in for the grant gateway: every sandbox named opens
// with one variable, SB_<NAME>, holding a secret value.
type stubHandoff struct {
	mu   sync.Mutex
	asks []string // "tenant stack workspaceID grantID sandbox,sandbox"
	err  error
}

const (
	stubToken  = "rg1.eyJnIjoicmdyXzEifQ.c2lnbmF0dXJlLXNpZ25hdHVyZQ"
	stubSecret = "ghp_secret-value-of-the-sandbox"
)

func (h *stubHandoff) ForExec(_ context.Context, tenant, stack, workspaceID, grantID string, sandboxes []string) (map[string]string, [][]byte, error) {
	h.mu.Lock()
	h.asks = append(h.asks, strings.Join([]string{tenant, stack, workspaceID, grantID, strings.Join(sandboxes, ",")}, " "))
	h.mu.Unlock()
	if h.err != nil {
		return nil, nil, h.err
	}
	env := map[string]string{
		"TXCO_RUN": "task-1", "TXCO_RUN_GRANT": stubToken, "TXCO_GRANT_SOCK": "/tmp/g.sock", "TXCO_BIN": "/bin/txco",
	}
	scrub := [][]byte{[]byte(stubToken)}
	for _, sb := range sandboxes {
		env["SB_"+strings.ToUpper(sb)] = stubSecret + "-" + sb
		scrub = append(scrub, []byte(stubSecret+"-"+sb))
	}
	return env, scrub, nil
}

func newGrantUnit(t *testing.T, stub *stubProvider, h GrantHandoff) (*Unit, context.Context) {
	t.Helper()
	pu, ctx := newWorkspaceUnit(t, stub)
	pu.Workspaces = workspace.NewManager(reachingProvider{stub}, workspace.Limits{}, nil)
	pu.Grants = h
	return pu, ctx
}

func TestExecWorkspaceGrantIsHandedToTheCommand(t *testing.T) {
	stub := &stubProvider{echoEnv: true}
	h := &stubHandoff{}
	pu, ctx := newGrantUnit(t, stub, h)

	// The rule is on the stack's mail channel; the workspace is the stack's.
	// It names two sandboxes, one as a string and one in a list elsewhere.
	op := workspaceOp("workspace://bench/exec",
		`{"command":"env","grant":"rgr_1","sandbox":["github","postgres"],"env":{"PLAIN":"kept","TXCO_RUN_GRANT":"mine","SB_GITHUB":"the rule's"}}`)
	op.Stack = "site/_mail"
	p, err := pu.ExecWorkspace(ctx, op)
	if err != nil {
		t.Fatalf("Go error: %v", err)
	}
	if code := gjson.Get(p.Raw, "workspace.error.code").String(); code != "" {
		t.Fatalf("in-band error: %s", p.Raw)
	}
	if want := "acme site/_mail " + workspace.ID("acme", "site", "bench") + " rgr_1 github,postgres"; len(h.asks) != 1 || h.asks[0] != want {
		t.Errorf("handoff asked for %q, want %q", h.asks, want)
	}

	// The command saw the sandboxes' variables and the chassis's — not the
	// rule's where they clash.
	stub.mu.Lock()
	env := stub.seen.Env
	stub.mu.Unlock()
	for k, want := range map[string]string{
		"TXCO_RUN": "task-1", "TXCO_RUN_GRANT": stubToken, "TXCO_GRANT_SOCK": "/tmp/g.sock", "TXCO_BIN": "/bin/txco",
		"SB_GITHUB": stubSecret + "-github", "SB_POSTGRES": stubSecret + "-postgres", "PLAIN": "kept",
	} {
		if env[k] != want {
			t.Errorf("the command's %s = %q, want %q", k, env[k], want)
		}
	}

	// …and nothing of them reached the envelope: `env` printed them all.
	for what, leak := range map[string]string{"the token": stubToken, "a value": stubSecret} {
		if strings.Contains(p.Raw, leak) {
			t.Errorf("the result holds %s: %s", what, p.Raw)
		}
	}
	out := gjson.Get(p.Raw, "_workspace.stdout").String()
	if !strings.Contains(out, "TXCO_RUN_GRANT=[REDACTED]") || !strings.Contains(out, "SB_GITHUB=[REDACTED]") ||
		!strings.Contains(out, "TXCO_RUN=task-1") || !strings.Contains(out, "PLAIN=kept") {
		t.Errorf("stdout = %q", out)
	}

	// One sandbox, as a string.
	h.asks = nil
	p, _ = pu.ExecWorkspace(ctx, workspaceOp("workspace://bench/exec", `{"command":"env","grant":"rgr_1","sandbox":"github"}`))
	if code := gjson.Get(p.Raw, "workspace.error.code").String(); code != "" || len(h.asks) != 1 || !strings.HasSuffix(h.asks[0], " rgr_1 github") {
		t.Errorf("one sandbox: %s asks=%v", p.Raw, h.asks)
	}

	// A provider whose commands run elsewhere: the push form works — the
	// values travel in the exec — and the command gets the run's name and
	// the token (what a node presents to its parent's capability inlet),
	// and not the socket or the binary, which it could not use there.
	for name, prov := range map[string]func(*stubProvider) workspace.Provider{
		"a provider without the capability": func(s *stubProvider) workspace.Provider { return s },
		"a provider that says it cannot":    func(s *stubProvider) workspace.Provider { return farProvider{s} },
	} {
		far := &stubProvider{echoEnv: true}
		pu2, ctx2 := newGrantUnit(t, far, h)
		pu2.Workspaces = workspace.NewManager(prov(far), workspace.Limits{}, nil)
		p, _ = pu2.ExecWorkspace(ctx2, workspaceOp("workspace://bench/exec", `{"command":"env","grant":"rgr_1","sandbox":"github"}`))
		if code := gjson.Get(p.Raw, "workspace.error.code").String(); code != "" {
			t.Fatalf("%s, the push form: %s", name, p.Raw)
		}
		far.mu.Lock()
		env = far.seen.Env
		far.mu.Unlock()
		if env["SB_GITHUB"] != stubSecret+"-github" || env["TXCO_RUN"] != "task-1" || env["TXCO_RUN_GRANT"] != stubToken {
			t.Errorf("%s: the far command's env = %v", name, env)
		}
		for _, k := range []string{"TXCO_GRANT_SOCK", "TXCO_BIN"} {
			if _, has := env[k]; has {
				t.Errorf("%s: the far command was given %s, which it cannot use", name, k)
			}
		}
		if strings.Contains(p.Raw, stubSecret) || strings.Contains(p.Raw, stubToken) {
			t.Errorf("%s: the result holds a value: %s", name, p.Raw)
		}
		// And with no sandbox at all: the token is still worth handing over.
		p, _ = pu2.ExecWorkspace(ctx2, workspaceOp("workspace://bench/exec", `{"command":"env","grant":"rgr_1"}`))
		far.mu.Lock()
		env = far.seen.Env
		far.mu.Unlock()
		if code := gjson.Get(p.Raw, "workspace.error.code").String(); code != "" || env["TXCO_RUN_GRANT"] != stubToken {
			t.Errorf("%s, no sandbox: %s env=%v", name, p.Raw, env)
		}
	}
}

func TestExecWorkspaceGrantIsScrubbedFromErrors(t *testing.T) {
	stub := &stubProvider{err: &workspace.Error{Code: "provider", Message: "spawn failed with TXCO_RUN_GRANT=" + stubToken + " and " + stubSecret + "-github"}}
	pu, ctx := newGrantUnit(t, stub, &stubHandoff{})
	p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://bench/exec", `{"command":"env","grant":"rgr_1","sandbox":"github"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Raw, stubToken) || strings.Contains(p.Raw, stubSecret) || !strings.Contains(p.Raw, "[REDACTED]") {
		t.Errorf("the error carries the grant: %s", p.Raw)
	}
}

func TestExecWorkspaceGrantRefusals(t *testing.T) {
	refused := errors.New(`grant: run grant rgr_1 was minted for workspace "other", not this one`)
	for name, tc := range map[string]struct {
		exec, meta string
		handoff    GrantHandoff
		provider   func(*stubProvider) workspace.Provider
		code, msg  string
	}{
		"the gateway refuses": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":"rgr_1"}`,
			handoff: &stubHandoff{err: refused}, code: "grant_refused", msg: "minted for workspace",
		},
		"no gateway on this node": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":"rgr_1"}`,
			code: "grant_unavailable", msg: "hands out no run grants",
		},
		"a sandbox and no grant": {
			exec: "workspace://bench/exec", meta: `{"command":"env","sandbox":"github"}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "needs WITH grant",
		},
		"a sandbox that is not a name": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":"rgr_1","sandbox":"../GitHub"}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "is not a sandbox name",
		},
		"a sandbox that is a number": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":"rgr_1","sandbox":7}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "must be a sandbox name or an array",
		},
		"a list with a number": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":"rgr_1","sandbox":["github",7]}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "must be a sandbox name or an array",
		},
		"an empty list": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":"rgr_1","sandbox":[]}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "names no sandbox",
		},
		"an empty grant": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":""}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "must be a run grant's id",
		},
		"a grant that is not a string": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":{"id":"rgr_1"}}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "must be a run grant's id",
		},
		"a token for a grant": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":true}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "must be a run grant's id",
		},
		"with stream": {
			exec: "workspace://bench/exec", meta: `{"command":"env","grant":"rgr_1","stream":true}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "cannot be combined with grant",
		},
		"on attach": {
			exec: "workspace://bench/attach", meta: `{"grant":"rgr_1"}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "attach hands a run grant to no command",
		},
		"on connect": {
			exec: "workspace://bench/connect", meta: `{"service":"browser","grant":"rgr_1"}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "connect hands a run grant to no command",
		},
		"on wake": {
			exec: "workspace://bench/wake", meta: `{"grant":"rgr_1"}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "wake hands a run grant to no command",
		},
		"on destroy": {
			exec: "workspace://bench/destroy", meta: `{"grant":"rgr_1"}`,
			handoff: &stubHandoff{}, code: "bad_request", msg: "destroy hands a run grant to no command",
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &stubProvider{echoEnv: true}
			pu, ctx := newGrantUnit(t, stub, tc.handoff)
			if tc.handoff == nil {
				pu.Grants = nil // a nil interface, as on a node that built none
			}
			if tc.provider != nil {
				pu.Workspaces = workspace.NewManager(tc.provider(stub), workspace.Limits{}, nil)
			}
			p, err := pu.ExecWorkspace(ctx, workspaceOp(tc.exec, tc.meta))
			if err != nil {
				t.Fatalf("Go error: %v", err)
			}
			if code := gjson.Get(p.Raw, "workspace.error.code").String(); code != tc.code {
				t.Errorf("code = %q, want %q: %s", code, tc.code, p.Raw)
			}
			if msg := gjson.Get(p.Raw, "workspace.error.message").String(); !strings.Contains(msg, tc.msg) {
				t.Errorf("message = %q, want it to say %q", msg, tc.msg)
			}
			// Nothing ran, and nothing was destroyed.
			if n := stub.execs.Load() + stub.destroys.Load(); n != 0 {
				t.Errorf("a refused grant still reached the provider %d times", n)
			}
			if h, ok := tc.handoff.(*stubHandoff); ok && h.err == nil && len(h.asks) != 0 {
				t.Errorf("a token was signed for a command that never ran: %v", h.asks)
			}
		})
	}
}

// An exec without `grant` is what it always was.
func TestExecWorkspaceWithoutGrantAsksForNone(t *testing.T) {
	stub := &stubProvider{echoEnv: true}
	h := &stubHandoff{}
	pu, ctx := newGrantUnit(t, stub, h)
	p, err := pu.ExecWorkspace(ctx, workspaceOp("workspace://bench/exec", `{"command":"env","env":{"PLAIN":"kept"}}`))
	if err != nil || gjson.Get(p.Raw, "workspace.error.code").String() != "" {
		t.Fatalf("exec: %s err=%v", p.Raw, err)
	}
	if len(h.asks) != 0 {
		t.Errorf("the gateway was asked for a grant nobody named: %v", h.asks)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for k := range stub.seen.Env {
		if strings.HasPrefix(k, "TXCO_") {
			t.Errorf("the command was given %s", k)
		}
	}
}

func TestScrubValues(t *testing.T) {
	vals := [][]byte{[]byte("a-long-token-value"), []byte("short"), nil}
	if got := string(scrubValues([]byte("x a-long-token-value y short z a-long-token-value"), vals)); got != "x [REDACTED] y short z [REDACTED]" {
		t.Errorf("scrub = %q", got)
	}
	if got := scrubValues(nil, vals); got != nil {
		t.Errorf("scrub of nothing = %q", got)
	}
	if got := string(scrubValues([]byte("untouched"), nil)); got != "untouched" {
		t.Errorf("scrub with no values = %q", got)
	}
}
