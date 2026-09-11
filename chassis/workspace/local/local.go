// Package local is the dev/self-host workspace provider: a workspace is a
// directory under --workspace-local-root, and exec runs the command as the
// chassis's own uid with that directory as HOME and cwd.
//
// There is NO isolation beyond a scrubbed environment and a cwd guard —
// the command can read anything the chassis uid can. That is why the
// chassis refuses this provider unless --workspace-allow-local is set
// explicitly, never implied by --env, and logs a WARN pair at boot.
//
//	import _ "github.com/loremlabs/thanks-computer/chassis/workspace/local"
package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

func init() {
	workspace.Register("local", func(cfg workspace.Config) (workspace.Provider, error) {
		return New(cfg.LocalRoot)
	})
}

// Provider keeps every workspace under root.
type Provider struct {
	root string
}

// New creates (if needed) and resolves the root directory. Symlinks in the
// root are resolved once here so the cwd guard compares real paths.
func New(root string) (*Provider, error) {
	if root == "" {
		return nil, errors.New("workspace/local: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace/local: root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("workspace/local: mkdir root: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace/local: root: %w", err)
	}
	return &Provider{root: real}, nil
}

// Root is the resolved root directory.
func (p *Provider) Root() string { return p.root }

func (p *Provider) Name() string           { return "local" }
func (p *Provider) Capabilities() []string { return []string{"exec"} }

// dirFor maps a spec to its directory and refuses anything that would
// leave root. Tenant and stack are chassis-validated slugs upstream; the
// checks here are belt-and-braces.
func (p *Provider) dirFor(spec workspace.Spec) (string, error) {
	if spec.Tenant == "" || spec.Stack == "" {
		return "", &workspace.Error{Code: "bad_request", Message: "workspace needs a tenant and a stack"}
	}
	for _, seg := range []string{spec.Tenant, spec.Stack} {
		if seg == "." || seg == ".." || strings.ContainsAny(seg, `/\`) {
			return "", &workspace.Error{Code: "bad_request", Message: fmt.Sprintf("bad path segment %q", seg)}
		}
	}
	if err := workspace.ValidateName(spec.Name); err != nil {
		return "", &workspace.Error{Code: "bad_request", Message: err.Error()}
	}
	dir := filepath.Join(p.root, spec.Tenant, spec.Stack, filepath.FromSlash(spec.Name))
	if !within(p.root, dir) {
		return "", &workspace.Error{Code: "bad_request", Message: "workspace path escapes the root"}
	}
	return dir, nil
}

// Create makes the directory (idempotent).
func (p *Provider) Create(_ context.Context, spec workspace.Spec) (workspace.Handle, error) {
	dir, err := p.dirFor(spec)
	if err != nil {
		return workspace.Handle{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return workspace.Handle{}, &workspace.Error{Code: "provider", Message: "mkdir: " + err.Error()}
	}
	return workspace.Handle{Provider: "local", Ref: dir, Name: spec.Name}, nil
}

// Wake checks the directory still exists and hands back a Computer bound
// to it. Bookkeeping only — a directory is never asleep.
func (p *Provider) Wake(_ context.Context, h workspace.Handle) (workspace.Computer, error) {
	if !within(p.root, h.Ref) {
		return nil, &workspace.Error{Code: "bad_request", Message: "handle outside the workspace root"}
	}
	st, err := os.Stat(h.Ref)
	if err != nil || !st.IsDir() {
		return nil, &workspace.Error{Code: "provider", Message: "workspace directory missing: " + h.Ref}
	}
	return &computer{dir: h.Ref}, nil
}

// Sleep is a no-op: nothing to park.
func (p *Provider) Sleep(context.Context, workspace.Handle) error { return nil }

// Destroy removes the directory. Guarded to the root so a forged handle
// cannot remove anything else.
func (p *Provider) Destroy(_ context.Context, h workspace.Handle) error {
	if !within(p.root, h.Ref) || filepath.Clean(h.Ref) == p.root {
		return &workspace.Error{Code: "bad_request", Message: "refusing to destroy outside the workspace root"}
	}
	if err := os.RemoveAll(h.Ref); err != nil {
		return &workspace.Error{Code: "provider", Message: "remove: " + err.Error()}
	}
	return nil
}

// Status is "running" while the directory exists (LastUsed = its mtime),
// "destroyed" otherwise.
func (p *Provider) Status(_ context.Context, h workspace.Handle) (workspace.Status, error) {
	st, err := os.Stat(h.Ref)
	if err != nil {
		if os.IsNotExist(err) {
			return workspace.Status{State: "destroyed"}, nil
		}
		return workspace.Status{}, &workspace.Error{Code: "provider", Message: err.Error()}
	}
	return workspace.Status{State: "running", LastUsed: st.ModTime()}, nil
}

// within reports whether p is root or beneath it (both already clean/real).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// computer runs commands in one workspace directory.
type computer struct {
	dir string
}

// basePath is the only PATH a command sees unless the request sets its own.
const basePath = "/usr/local/bin:/usr/bin:/bin"

// argvFor turns a request into an argv: Command → the workspace shell,
// Args → verbatim. Exactly one must be set.
func argvFor(req workspace.ExecRequest) ([]string, error) {
	switch {
	case req.Command != "" && len(req.Args) > 0:
		return nil, &workspace.Error{Code: "bad_request", Message: "set command or args, not both"}
	case req.Command != "":
		return []string{"/bin/sh", "-c", req.Command}, nil
	case len(req.Args) > 0:
		if req.Args[0] == "" {
			return nil, &workspace.Error{Code: "bad_request", Message: "args[0] is empty"}
		}
		return req.Args, nil
	default:
		return nil, &workspace.Error{Code: "bad_request", Message: "no command or args"}
	}
}

// resolveCwd joins a relative cwd onto the workspace and proves the
// result — after symlink resolution — is still inside it.
func resolveCwd(root, cwd string) (string, error) {
	if filepath.IsAbs(cwd) {
		return "", &workspace.Error{Code: "bad_request", Message: "cwd must be relative to the workspace"}
	}
	target := filepath.Clean(filepath.Join(root, cwd))
	if !within(root, target) {
		return "", &workspace.Error{Code: "bad_request", Message: "cwd escapes the workspace"}
	}
	real, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", &workspace.Error{Code: "bad_request", Message: "cwd: " + err.Error()}
	}
	if !within(root, real) {
		return "", &workspace.Error{Code: "bad_request", Message: "cwd escapes the workspace (symlink)"}
	}
	st, err := os.Stat(real)
	if err != nil || !st.IsDir() {
		return "", &workspace.Error{Code: "bad_request", Message: "cwd is not a directory"}
	}
	return real, nil
}

// envFor is the scrubbed environment: a fixed PATH, HOME at the workspace,
// TMPDIR inside it, then the request's own variables verbatim (which may
// override any of those). Never the chassis process environment.
func envFor(dir, tmp string, extra map[string]string) []string {
	base := map[string]string{
		"PATH":   basePath,
		"HOME":   dir,
		"TMPDIR": tmp,
	}
	for k, v := range extra {
		base[k] = v
	}
	return workspace.SortedEnv(base)
}

// Exec runs one command. The op's ctx bounds it: on ctx done the whole
// process group is killed (Setpgid) and ErrTimeout is returned with
// Exit = -1. A non-zero exit is NOT an error — it is data in the result.
func (c *computer) Exec(ctx context.Context, req workspace.ExecRequest, lim workspace.Limits) (workspace.ExecResult, error) {
	argv, err := argvFor(req)
	if err != nil {
		return workspace.ExecResult{Exit: -1}, err
	}
	cwd := c.dir
	if req.Cwd != "" {
		if cwd, err = resolveCwd(c.dir, req.Cwd); err != nil {
			return workspace.ExecResult{Exit: -1}, err
		}
	}
	tmp := filepath.Join(c.dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return workspace.ExecResult{Exit: -1}, &workspace.Error{Code: "provider", Message: "mkdir tmp: " + err.Error()}
	}

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = envFor(c.dir, tmp, req.Env)
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	// Streaming (WITH stream): stdout goes straight to the caller's writer
	// as os/exec's copy goroutine reads the pipe, so the client sees output
	// while the command is still running. Otherwise it is captured + capped.
	stdout := workspace.NewCappedWriter(lim.MaxOutputBytes)
	if req.StdoutTo != nil {
		stdout = workspace.NewStreamWriter(req.StdoutTo)
	}
	stderr := workspace.NewCappedWriter(lim.MaxOutputBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Own process group so a timeout kills the command AND anything it
	// spawned, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	res := workspace.ExecResult{
		Stdout:          stdout.Bytes(),
		Stderr:          stderr.Bytes(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
		StdoutBytes:     stdout.Count(),
		WallMS:          time.Since(start).Milliseconds(),
	}
	switch {
	case runErr == nil:
		return res, nil
	case ctx.Err() != nil:
		res.Exit = -1
		return res, workspace.ErrTimeout
	case errors.Is(runErr, exec.ErrWaitDelay):
		// The command exited but a grandchild held the output pipe past
		// WaitDelay; the exit status is still known.
		res.Exit = cmd.ProcessState.ExitCode()
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(runErr, &ee) {
		res.Exit = ee.ExitCode() // -1 when killed by a signal
		return res, nil
	}
	// Start failure: argv[0] not found, cwd vanished, fork failure.
	res.Exit = -1
	return res, &workspace.Error{Code: "bad_request", Message: runErr.Error()}
}
