package local

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/attached/home"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
	"github.com/loremlabs/thanks-computer/chassis/workspace/detach"
)

// The OWNER provider is what `txco attach` runs commands with: the local
// provider's exec, with three things changed because the machine is its
// owner's and not a sandbox the chassis keeps.
//
//   - A workspace name is a credential id this machine holds. Its directory
//     is that credential's ROOT: the folder `txco attach <code>` was run
//     in. Any other name is not found.
//   - The environment is the owner's own, as `txco attach` was started with
//     it — their HOME and PATH, where their logins live — then the
//     request's variables, then TXCO_ATTACH_ROOT and TXCO_ATTACH_STATE.
//   - `cwd` starts at the root and is not held under it: an absolute path is
//     used as given. The root is where work starts, not a fence.
//
// Everything else is the local provider's own code: the process group, the
// kill on cancel, the output caps, the exit code as data, and detached jobs.
//
// It serves ONE tenant, the chassis's own `_attach` (ServesTenant). An
// owner's `_attach` stack may shape what runs; no other stack on the machine
// can run anything through it.
func init() {
	workspace.Register(OwnerName, func(cfg workspace.Config) (workspace.Provider, error) {
		return NewOwner(home.Home{Dir: cfg.AttachHome}, nil)
	})
}

// OwnerName is the owner provider's name (--workspace-provider).
const OwnerName = "owner"

// OwnerTenant is the one tenant the owner provider serves.
const OwnerTenant = "_attach"

// Environment variables every owner-mode command is given, set last.
const (
	EnvAttachRoot  = "TXCO_ATTACH_ROOT"
	EnvAttachState = "TXCO_ATTACH_STATE"
)

var (
	ownerEnvMu sync.Mutex
	ownerEnv   []string
)

// SetOwnerEnv records the environment owner-mode commands start from.
// `txco attach` calls it with its own environment as it was started, before
// it clears the chassis's settings from the process.
func SetOwnerEnv(env []string) {
	ownerEnvMu.Lock()
	defer ownerEnvMu.Unlock()
	ownerEnv = append([]string(nil), env...)
}

func baseOwnerEnv() []string {
	ownerEnvMu.Lock()
	defer ownerEnvMu.Unlock()
	if ownerEnv != nil {
		return append([]string(nil), ownerEnv...)
	}
	return os.Environ()
}

// Owner is the owner provider.
type Owner struct {
	home home.Home
	jobs *detach.Store
}

// NewOwner builds the provider over an attach folder. wrapper is the argv
// that runs a detached job's wrapper; nil is this binary's own.
func NewOwner(h home.Home, wrapper []string) (*Owner, error) {
	if h.Dir == "" {
		return nil, &workspace.Error{Code: "bad_request", Message: "the owner provider needs the attach folder (--attach-home)"}
	}
	jobs, err := detach.New(h.JobsDir(), wrapper)
	if err != nil {
		return nil, err
	}
	return &Owner{home: h, jobs: jobs}, nil
}

func (o *Owner) Name() string           { return OwnerName }
func (o *Owner) Capabilities() []string { return []string{"exec", "detach"} }

// Jobs is the store of this machine's detached jobs.
func (o *Owner) Jobs() *detach.Store { return o.jobs }

// ServesTenant implements workspace.TenantGate.
func (o *Owner) ServesTenant(slug string) bool { return slug == OwnerTenant }

// ReachesGrants: the commands run on this machine, but the run they belong
// to is another chassis's. Its grant arrives in the request's environment.
func (o *Owner) ReachesGrants() bool { return false }

// credential finds the live credential a name stands for.
func (o *Owner) credential(name string) (home.Credential, error) {
	c, ok := o.home.Get(name)
	if !ok || c.Ended != "" {
		return home.Credential{}, &workspace.Error{Code: workspace.CodeNotFound, Message: "this machine holds no workspace named " + name}
	}
	return c, nil
}

// Create names an existing credential: nothing is made.
func (o *Owner) Create(_ context.Context, spec workspace.Spec) (workspace.Handle, error) {
	if _, err := o.credential(spec.Name); err != nil {
		return workspace.Handle{}, err
	}
	return workspace.Handle{Provider: OwnerName, Ref: spec.Name, Name: spec.Name}, nil
}

// Wake hands back a computer bound to the credential's root. The root is
// read from the credential's file each time: it is the one place it lives.
func (o *Owner) Wake(_ context.Context, h workspace.Handle) (workspace.Computer, error) {
	c, err := o.credential(h.Ref)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(c.Root)
	if err != nil || !st.IsDir() {
		return nil, &workspace.Error{Code: "unavailable", Message: "the workspace's folder is gone: " + c.Root}
	}
	state := o.home.StateDir(c.ID)
	if err := os.MkdirAll(state, 0o700); err != nil {
		return nil, &workspace.Error{Code: "provider", Message: "mkdir state: " + err.Error()}
	}
	return &ownerComputer{root: c.Root, state: state, jobs: o.jobs}, nil
}

func (o *Owner) Sleep(context.Context, workspace.Handle) error { return nil }

// Destroy removes nothing. The folder is its owner's.
func (o *Owner) Destroy(context.Context, workspace.Handle) error { return nil }

func (o *Owner) Status(_ context.Context, h workspace.Handle) (workspace.Status, error) {
	if _, err := o.credential(h.Ref); err != nil {
		return workspace.Status{State: "destroyed"}, nil
	}
	return workspace.Status{State: "running"}, nil
}

type ownerComputer struct {
	root  string
	state string
	jobs  *detach.Store
}

// ownerCwd is where a command starts: the root, a directory below it, or an
// absolute path as given. It must exist; a command is never started
// somewhere else instead.
func ownerCwd(root, cwd string) (string, error) {
	target := root
	switch {
	case cwd == "":
	case filepath.IsAbs(cwd):
		target = filepath.Clean(cwd)
	default:
		target = filepath.Join(root, cwd)
	}
	st, err := os.Stat(target)
	if err != nil || !st.IsDir() {
		return "", &workspace.Error{Code: "bad_request", Message: "cwd is not a directory: " + target}
	}
	return target, nil
}

// env is the owner's environment, then the request's, then the two names
// every command can rely on.
func (c *ownerComputer) env(extra map[string]string) []string {
	m := map[string]string{}
	for _, kv := range baseOwnerEnv() {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				m[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	for k, v := range extra {
		m[k] = v
	}
	m[EnvAttachRoot] = c.root
	m[EnvAttachState] = c.state
	return workspace.SortedEnv(m)
}

func (c *ownerComputer) prepare(req workspace.ExecRequest) ([]string, string, error) {
	if req.TTY || req.StdoutTo != nil {
		return nil, "", &workspace.Error{Code: "unsupported", Message: "an attached machine has no terminal and no streamed output yet"}
	}
	argv, err := argvFor(req)
	if err != nil {
		return nil, "", err
	}
	cwd, err := ownerCwd(c.root, req.Cwd)
	if err != nil {
		return nil, "", err
	}
	return argv, cwd, nil
}

func (c *ownerComputer) Exec(ctx context.Context, req workspace.ExecRequest, lim workspace.Limits) (workspace.ExecResult, error) {
	argv, cwd, err := c.prepare(req)
	if err != nil {
		return workspace.ExecResult{Exit: -1}, err
	}
	return runCommand(ctx, argv, cwd, c.env(req.Env), req, lim)
}

// ExecDetached implements workspace.Detacher.
func (c *ownerComputer) ExecDetached(ctx context.Context, req workspace.ExecRequest, _ workspace.Limits) (workspace.DetachedJob, error) {
	if req.Timeout <= 0 {
		return workspace.DetachedJob{}, &workspace.Error{Code: "bad_request", Message: "a detached exec needs a timeout: it is the job's life"}
	}
	argv, cwd, err := c.prepare(req)
	if err != nil {
		return workspace.DetachedJob{}, err
	}
	job, err := c.jobs.Start(ctx, detach.Spec{
		Argv: argv, Dir: cwd, Env: c.env(req.Env), Deadline: time.Now().Add(req.Timeout),
	}, req.Stdin)
	if err != nil {
		return workspace.DetachedJob{}, err
	}
	return workspace.DetachedJob{ID: job.ID, StartedAt: job.StartedAt}, nil
}
