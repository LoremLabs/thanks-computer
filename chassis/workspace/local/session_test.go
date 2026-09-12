package local

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// startSession is a small helper: Start a session and fail on error.
func startSession(t *testing.T, c workspace.Computer, req workspace.ExecRequest) workspace.ExecSession {
	t.Helper()
	st, ok := c.(workspace.Starter)
	if !ok {
		t.Fatal("local computer does not implement Starter")
	}
	sess, err := st.Start(context.Background(), req, workspace.Limits{})
	if err != nil {
		t.Fatalf("Start %+v: %v", req, err)
	}
	return sess
}

// TestTTYAllocatesTerminal proves `tty = true` gives the guest a real
// terminal: test -t 0 succeeds and stty reports the requested geometry.
func TestTTYAllocatesTerminal(t *testing.T) {
	_, c, _ := newComputer(t)
	sess := startSession(t, c, workspace.ExecRequest{
		Command: "test -t 0 && test -t 1 && stty size",
		TTY:     true, Cols: 120, Rows: 40,
	})
	out, _ := io.ReadAll(sess.Stdout())
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Exit != 0 {
		t.Fatalf("exit = %d, want 0 (not a tty); output %q", res.Exit, out)
	}
	if got := strings.TrimSpace(string(out)); got != "40 120" {
		t.Errorf("stty size = %q, want \"40 120\"", got)
	}
	if len(res.Stderr) != 0 {
		t.Errorf("stderr = %q, want empty on a tty session", res.Stderr)
	}
}

// TestTTYMergesStderr: on a terminal there is one stream, so a program's
// stderr comes back in stdout and ExecResult.Stderr stays empty.
func TestTTYMergesStderr(t *testing.T) {
	_, c, _ := newComputer(t)
	sess := startSession(t, c, workspace.ExecRequest{
		Command: "echo out; echo err >&2", TTY: true,
	})
	out, _ := io.ReadAll(sess.Stdout())
	res, _ := sess.Wait()
	s := string(out)
	if !strings.Contains(s, "out") || !strings.Contains(s, "err") {
		t.Errorf("merged output = %q, want both out and err", s)
	}
	if len(res.Stderr) != 0 {
		t.Errorf("stderr = %q, want empty", res.Stderr)
	}
}

// TestNonTTYSessionSplitsStreamsAndStdin: without a tty, stdin is a pipe,
// stdout is drained by the caller, stderr is captured on the result.
func TestNonTTYSessionSplitsStreamsAndStdin(t *testing.T) {
	_, c, _ := newComputer(t)
	sess := startSession(t, c, workspace.ExecRequest{
		Command: "cat; echo done >&2",
	})
	go func() {
		_, _ = io.WriteString(sess.Stdin(), "hello over stdin")
		_ = sess.Stdin().Close()
	}()
	out, _ := io.ReadAll(sess.Stdout())
	res, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.Exit != 0 {
		t.Fatalf("exit = %d", res.Exit)
	}
	if string(out) != "hello over stdin" {
		t.Errorf("stdout = %q, want the stdin echoed back", out)
	}
	if strings.TrimSpace(string(res.Stderr)) != "done" {
		t.Errorf("stderr = %q, want \"done\"", res.Stderr)
	}
}

// TestSessionResizeMidRun: a resize after start reaches the running program.
func TestSessionResizeMidRun(t *testing.T) {
	_, c, _ := newComputer(t)
	// trap SIGWINCH, print the new size, exit. A resize fires WINCH.
	sess := startSession(t, c, workspace.ExecRequest{
		Command: `trap 'stty size; exit 0' WINCH; sleep 5 & wait`,
		TTY:     true, Cols: 80, Rows: 24,
	})
	// Give the shell a beat to install the trap.
	time.Sleep(200 * time.Millisecond)
	if err := sess.Resize(100, 50); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	out, _ := io.ReadAll(sess.Stdout())
	sess.Wait()
	if got := strings.TrimSpace(string(out)); got != "50 100" {
		t.Errorf("size after resize = %q, want \"50 100\"", got)
	}
}

// TestSessionSignal: a named signal reaches the process group.
func TestSessionSignal(t *testing.T) {
	_, c, _ := newComputer(t)
	sess := startSession(t, c, workspace.ExecRequest{
		Command: `trap 'echo got-int; exit 7' INT; sleep 5 & wait`,
		TTY:     true,
	})
	time.Sleep(200 * time.Millisecond)
	if err := sess.Signal("INT"); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	out, _ := io.ReadAll(sess.Stdout())
	res, _ := sess.Wait()
	if !strings.Contains(string(out), "got-int") {
		t.Errorf("output = %q, want the trap to have fired", out)
	}
	if res.Exit != 7 {
		t.Errorf("exit = %d, want 7", res.Exit)
	}
	if err := sess.Signal("INT"); err == nil {
		t.Error("Signal after exit should error")
	}
}

// TestSessionCloseKillsProcess: Close ends a running session promptly.
func TestSessionCloseKillsProcess(t *testing.T) {
	_, c, _ := newComputer(t)
	sess := startSession(t, c, workspace.ExecRequest{Command: "sleep 30", TTY: true})
	done := make(chan struct{})
	go func() { sess.Wait(); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if err := sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Close")
	}
}

// TestSessionBadSignalRejected: an unknown signal name is a bad request.
func TestSessionBadSignalRejected(t *testing.T) {
	_, c, _ := newComputer(t)
	sess := startSession(t, c, workspace.ExecRequest{Command: "sleep 1"})
	defer sess.Close()
	if err := sess.Signal("BOGUS"); err == nil {
		t.Fatal("Signal(BOGUS) should error")
	}
	if err := sess.Resize(80, 24); err == nil {
		t.Fatal("Resize on a non-tty session should error")
	}
}
