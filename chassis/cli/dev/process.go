// Package dev contains the process-spawning, health-checking, and
// file-watching helpers used by `txco dev`. Lives outside chassis/cli
// proper so the dev subcommand's machinery doesn't bleed into the
// simpler subcommands.
package dev

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Process is a managed child started by `txco dev`. It captures the
// child's stdout/stderr to a tagged writer (so output is interleaved
// readably) and runs it in its own process group so a single SIGTERM
// from the parent kills any grand-children too.
type Process struct {
	Name string
	Cmd  *exec.Cmd

	mu     sync.Mutex
	exited bool
	err    error
	doneCh chan struct{}
}

// SpawnConfig drives Spawn.
type SpawnConfig struct {
	Name string    // tag prefixed to output lines (e.g., "[classify]")
	Dir  string    // working dir; "" = current
	Cmd  string    // shell command, e.g. "npm run dev"
	Out  io.Writer // where to write tagged stdout/stderr
	Env  []string  // extra env vars (appended to os.Environ())
}

// Spawn starts a child process running `sh -c <cmd>` with the given
// working dir, in its own process group. The child's combined output is
// line-prefixed with cfg.Name and written to cfg.Out.
//
// Returns a Process whose Wait() blocks until the child exits, and whose
// Stop(grace) sends SIGTERM to the whole group, then SIGKILL after grace.
func Spawn(ctx context.Context, cfg SpawnConfig) (*Process, error) {
	if cfg.Cmd == "" {
		return nil, errors.New("dev: empty command")
	}
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}

	c := exec.CommandContext(ctx, "sh", "-c", cfg.Cmd)
	if cfg.Dir != "" {
		c.Dir = cfg.Dir
	}
	c.Env = append(os.Environ(), cfg.Env...)
	// Run in its own process group so we can SIGTERM the whole subtree.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := c.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("start %q: %w", cfg.Name, err)
	}

	p := &Process{
		Name:   cfg.Name,
		Cmd:    c,
		doneCh: make(chan struct{}),
	}

	go pumpLines(stdout, cfg.Out, "["+cfg.Name+"] ")
	go pumpLines(stderr, cfg.Out, "["+cfg.Name+"] ")

	go func() {
		err := c.Wait()
		p.mu.Lock()
		p.exited = true
		p.err = err
		p.mu.Unlock()
		close(p.doneCh)
	}()

	return p, nil
}

// Done returns a channel closed when the child exits.
func (p *Process) Done() <-chan struct{} { return p.doneCh }

// Err returns the child's exit error (if any). Only meaningful after
// Done has fired.
func (p *Process) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Stop sends SIGTERM to the process group, then SIGKILL after grace if
// the child hasn't exited. Safe to call multiple times.
//
// Always signals the *group* (negative pid), even when the immediate
// child has already been reaped. The leader (sh) can die first while
// grandchildren (e.g. pnpm → vite) keep running in the same pgid —
// this happens when exec.CommandContext's ctx is cancelled, since
// os/exec SIGKILLs only the leader. Targeting -pid kills the whole
// subtree.
func (p *Process) Stop(grace time.Duration) {
	p.mu.Lock()
	exited := p.exited
	pid := p.Cmd.Process.Pid
	p.mu.Unlock()

	// Negative pid = kill the whole process group (Setpgid above made
	// the child the leader of a new group whose pgid == pid). Ignore
	// ESRCH — if the group is fully gone, this is a no-op.
	_ = syscall.Kill(-pid, syscall.SIGTERM)

	if exited {
		// Leader already reaped. We've best-effort-signalled any
		// stragglers; there's no doneCh to wait on for them.
		return
	}

	select {
	case <-p.doneCh:
		// Leader exited. Grandchildren in the same group also got the
		// SIGTERM above and should be dying. We don't wait further —
		// callers expecting "Stop returns ⇒ everything is dead" pay a
		// small race against fast-exiting node servers; in practice
		// vite/pnpm exit on SIGTERM in tens of milliseconds.
	case <-time.After(grace):
	}

	p.mu.Lock()
	exited = p.exited
	p.mu.Unlock()
	if !exited {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		<-p.doneCh
	}
}

// Signal sends sig to the child's process group (negative pid), mirroring
// Stop's group-targeting so a forwarded signal reaches the real process
// even when a shell leader sits in front of it. Used to forward operator
// signals (e.g. SIGUSR1/SIGUSR2 drain) from the dev supervisor to a
// specific child. Returns the underlying kill error (ESRCH if the group
// is already gone).
func (p *Process) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	pid := p.Cmd.Process.Pid
	p.mu.Unlock()
	return syscall.Kill(-pid, sig)
}

// pumpLines reads lines from r and writes them to w, each prefixed with
// tag. Long lines are passed through; reader errors are silently
// swallowed (the parent process is exiting anyway).
func pumpLines(r io.Reader, w io.Writer, tag string) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			_, _ = io.WriteString(w, tag+line)
		}
		if err != nil {
			return
		}
	}
}
