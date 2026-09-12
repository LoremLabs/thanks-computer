package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// Start implements workspace.Starter: the same command, cwd guard and
// scrubbed environment as Exec, but the process is handed back running.
// With req.TTY the command gets a pseudo-terminal (github.com/creack/pty):
// stdin, stdout and stderr are the one terminal, so output comes back
// merged and stderr is never captured. Without it, stdin and stdout are
// pipes and stderr is captured and capped as on the one-shot path.
//
// Three things differ from Exec, each a quiet-breakage candidate:
//
//   - pty.Start calls cmd.Start itself, so the single cmd.Run splits into
//     Start here and Wait in the session's own goroutine;
//   - a controlling terminal implies a new session (Setsid + Setctty), which
//     cannot also carry Exec's Setpgid — but a session leader IS a
//     process-group leader with pgid == pid, so Kill(-pid) still reaches the
//     whole group, exactly as the timeout path relies on;
//   - the ctx bounds the session, not a call: when it ends the group is
//     KILLed and Wait reports ErrTimeout, as an exec's would.
func (c *computer) Start(ctx context.Context, req workspace.ExecRequest, lim workspace.Limits) (workspace.ExecSession, error) {
	argv, cwd, tmp, err := c.prepare(req)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = cwd
	env := req.Env
	if req.TTY {
		env = workspace.TTYEnv(env)
	}
	cmd.Env = envFor(c.dir, tmp, env)
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second

	s := &session{ctx: ctx, cmd: cmd, tty: req.TTY, start: time.Now(), done: make(chan struct{})}
	if req.TTY {
		cols, rows := req.Geometry()
		cmd.SysProcAttr = &syscall.SysProcAttr{} // pty sets Setsid + Setctty
		ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
		if err != nil {
			return nil, &workspace.Error{Code: "bad_request", Message: err.Error()}
		}
		s.ptmx = ptmx
		s.stdin = ptyStdin{ptmx}
		s.stdout = ptyReader{ptmx}
		s.closers = []io.Closer{ptmx}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// Our own pipes rather than cmd.Std{in,out}Pipe: os/exec closes those
		// itself in Wait, which would race the caller's reads. Handing the
		// child *os.Files also means no copy goroutines, so Wait is the
		// process's exit and nothing else.
		inR, inW, err := os.Pipe()
		if err != nil {
			return nil, &workspace.Error{Code: "provider", Message: "pipe: " + err.Error()}
		}
		outR, outW, err := os.Pipe()
		if err != nil {
			inR.Close()
			inW.Close()
			return nil, &workspace.Error{Code: "provider", Message: "pipe: " + err.Error()}
		}
		cmd.Stdin, cmd.Stdout = inR, outW
		s.stderr = workspace.NewCappedWriter(lim.MaxOutputBytes)
		cmd.Stderr = s.stderr
		if err := cmd.Start(); err != nil {
			for _, f := range []*os.File{inR, inW, outR, outW} {
				f.Close()
			}
			return nil, &workspace.Error{Code: "bad_request", Message: err.Error()}
		}
		// The child holds its ends now; ours must go so EOF propagates.
		inR.Close()
		outW.Close()
		s.stdin, s.stdout = inW, outR
		s.closers = []io.Closer{inW, outR}
	}
	go s.wait()
	return s, nil
}

// session is one running local process.
type session struct {
	ctx    context.Context
	cmd    *exec.Cmd
	tty    bool
	ptmx   *os.File
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *workspace.CappedWriter
	start  time.Time

	closers []io.Closer

	done      chan struct{} // closed once Wait's verdict is in
	res       workspace.ExecResult
	err       error
	closeOnce sync.Once
}

// wait is the one cmd.Wait; Wait and Close both consume its verdict.
func (s *session) wait() {
	runErr := s.cmd.Wait()
	res := workspace.ExecResult{WallMS: time.Since(s.start).Milliseconds()}
	if s.stderr != nil {
		res.Stderr = s.stderr.Bytes()
		res.StderrTruncated = s.stderr.Truncated()
	}
	s.res, s.err = mapRunErr(s.ctx, s.cmd, runErr, res)
	close(s.done)
}

func (s *session) Stdin() io.WriteCloser { return s.stdin }
func (s *session) Stdout() io.Reader     { return s.stdout }

func (s *session) Resize(cols, rows uint16) error {
	if !s.tty {
		return &workspace.Error{Code: "bad_request", Message: "resize: not a tty session"}
	}
	if cols == 0 || rows == 0 {
		return &workspace.Error{Code: "bad_request", Message: "resize: cols and rows must be positive"}
	}
	if err := pty.Setsize(s.ptmx, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		return &workspace.Error{Code: "provider", Message: "resize: " + err.Error()}
	}
	return nil
}

var signals = map[string]syscall.Signal{
	"INT": syscall.SIGINT, "TERM": syscall.SIGTERM, "HUP": syscall.SIGHUP, "KILL": syscall.SIGKILL,
	"QUIT": syscall.SIGQUIT, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

// Signal delivers to the whole process group (the shell and whatever it
// spawned), the same target the timeout kill uses.
func (s *session) Signal(sig string) error {
	sg, ok := signals[sig]
	if !ok {
		return &workspace.Error{Code: "bad_request", Message: fmt.Sprintf("signal: unknown %q (want one of %v)", sig, workspace.ValidSignals)}
	}
	select {
	case <-s.done:
		return &workspace.Error{Code: "bad_request", Message: "signal: process has exited"}
	default:
	}
	if err := syscall.Kill(-s.cmd.Process.Pid, sg); err != nil && !errors.Is(err, syscall.ESRCH) {
		return &workspace.Error{Code: "provider", Message: "signal: " + err.Error()}
	}
	return nil
}

func (s *session) Wait() (workspace.ExecResult, error) {
	<-s.done
	return s.res, s.err
}

// Close KILLs the group if it is still running, drops the transport (a
// closed pty master hangs the session up and unblocks any pending read),
// and waits briefly for the exit to be reaped.
func (s *session) Close() error {
	s.closeOnce.Do(func() {
		select {
		case <-s.done:
		default:
			if p := s.cmd.Process; p != nil {
				_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
			}
		}
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
		for _, c := range s.closers {
			_ = c.Close()
		}
	})
	return nil
}

// ptyStdin writes to the terminal; Close is a no-op because a pty has no
// EOF to deliver (closing the master would hang up the whole session).
type ptyStdin struct{ f *os.File }

func (w ptyStdin) Write(p []byte) (int, error) { return w.f.Write(p) }
func (w ptyStdin) Close() error                { return nil }

// ptyReader reads the terminal and reports the end of the session as EOF:
// Linux answers EIO once the slave side is gone, and a master we closed
// ourselves answers ErrClosed; either is "no more output".
type ptyReader struct{ f *os.File }

func (r ptyReader) Read(p []byte) (int, error) {
	n, err := r.f.Read(p)
	if err != nil && (errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)) {
		return n, io.EOF
	}
	return n, err
}
