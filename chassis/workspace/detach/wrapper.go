package detach

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// stopPoll is how often the wrapper looks for the job's stop file.
const stopPoll = 200 * time.Millisecond

// RunJob is the wrapper: the body of `txco __job <dir>`. It runs the job
// described in dir to its end and records the exit there. Its return value
// is the wrapper's own exit status, which nothing reads: the job's outcome
// is exit.json.
//
// It is safe to run twice on one directory. The second finds the lock held,
// or the exit already written, and does nothing.
func RunJob(dir string) int {
	// The starter's terminal may close; that is not this job's business.
	signal.Ignore(syscall.SIGHUP)

	lock, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "job lock:", err)
		return 2
	}
	// Go opens files close-on-exec, so the command never inherits the lock:
	// only this process can hold it, and it is free the moment we are gone.
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "job lock held by another wrapper:", err)
		return 3
	}
	if _, done := readExit(dir); done {
		return 0
	}

	var spec Spec
	raw, err := os.ReadFile(filepath.Join(dir, specFile))
	if err == nil {
		err = json.Unmarshal(raw, &spec)
	}
	if err == nil && len(spec.Argv) == 0 {
		err = errors.New("empty argv")
	}
	started := time.Now()
	if err != nil {
		return record(dir, Exit{Exit: -1, StartError: "job spec: " + err.Error(), StartedAt: started, EndedAt: started})
	}
	if _, err := os.Stat(filepath.Join(dir, stopFile)); err == nil {
		// Asked to stop before anything ran (the starter gave up on us).
		return record(dir, Exit{Exit: -1, Stopped: true, StartedAt: started, EndedAt: started})
	}

	maxLog := spec.MaxLogBytes
	if maxLog <= 0 {
		maxLog = DefaultMaxLogBytes
	}
	grace := time.Duration(spec.KillGraceMS) * time.Millisecond
	if grace <= 0 {
		grace = DefaultKillGrace
	}
	stdout, err := newCappedFile(filepath.Join(dir, stdoutFile), maxLog)
	if err != nil {
		return record(dir, Exit{Exit: -1, StartError: "job stdout: " + err.Error(), StartedAt: started, EndedAt: started})
	}
	defer stdout.Close()
	stderr, err := newCappedFile(filepath.Join(dir, stderrFile), maxLog)
	if err != nil {
		return record(dir, Exit{Exit: -1, StartError: "job stderr: " + err.Error(), StartedAt: started, EndedAt: started})
	}
	defer stderr.Close()

	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{} // never this wrapper's own environment
	}
	if spec.HasStdin {
		in, err := os.Open(filepath.Join(dir, stdinFile))
		if err != nil {
			return record(dir, Exit{Exit: -1, StartError: "job stdin: " + err.Error(), StartedAt: started, EndedAt: started})
		}
		defer in.Close()
		cmd.Stdin = in
	}
	// The outputs pass through this process so they can be capped: a command
	// that writes without end must not fill the owner's disk.
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// A group of its own, so the deadline and a stop end the command AND
	// whatever it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A grandchild may hold the output open past the command's exit; the
	// exit status is still known then.
	cmd.WaitDelay = 2 * time.Second

	started = time.Now()
	if err := cmd.Start(); err != nil {
		return record(dir, Exit{Exit: -1, StartError: err.Error(), StartedAt: started, EndedAt: time.Now()})
	}
	pgid := cmd.Process.Pid
	st, _ := json.Marshal(Started{WrapperPID: os.Getpid(), CommandPID: pgid, StartedAt: started})
	if err := writeAtomic(dir, startedFile, st); err != nil {
		fmt.Fprintln(os.Stderr, "job started:", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigs)

	var deadline <-chan time.Time
	if !spec.Deadline.IsZero() {
		t := time.NewTimer(time.Until(spec.Deadline))
		defer t.Stop()
		deadline = t.C
	}
	poll := time.NewTicker(stopPoll)
	defer poll.Stop()

	var (
		timedOut, stopped bool
		kill              <-chan time.Time
		waitErr           error
	)
	// end sends TERM to the command's group, once, and arms the KILL.
	end := func() {
		if kill != nil {
			return
		}
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		kill = time.After(grace)
	}
loop:
	for {
		select {
		case waitErr = <-waited:
			break loop
		case <-deadline:
			deadline = nil
			if !stopped {
				timedOut = true
			}
			end()
		case <-sigs:
			if !timedOut {
				stopped = true
			}
			end()
		case <-poll.C:
			if kill == nil {
				if _, err := os.Stat(filepath.Join(dir, stopFile)); err == nil {
					stopped = true
					end()
				}
			}
		case <-kill:
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	if timedOut || stopped {
		// The command is gone; anything it left in its group goes too.
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}

	ended := time.Now()
	ex := Exit{
		Exit:            -1,
		TimedOut:        timedOut,
		Stopped:         stopped,
		StartedAt:       started,
		EndedAt:         ended,
		WallMS:          ended.Sub(started).Milliseconds(),
		StdoutBytes:     stdout.Count(),
		StderrBytes:     stderr.Count(),
		StdoutTruncated: stdout.Truncated(),
		StderrTruncated: stderr.Truncated(),
	}
	if ps := cmd.ProcessState; ps != nil {
		ex.Exit = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			ex.Signal = ws.Signal().String()
		}
	} else if waitErr != nil {
		fmt.Fprintln(os.Stderr, "job wait:", waitErr)
	}
	_ = stdout.Sync()
	_ = stderr.Sync()
	return record(dir, ex)
}

// record writes the exit and returns the wrapper's own status. The lock is
// still held here: Status relies on the exit being there before the lock is
// free.
func record(dir string, ex Exit) int {
	b, _ := json.Marshal(ex)
	if err := writeAtomic(dir, exitFile, b); err != nil {
		fmt.Fprintln(os.Stderr, "job exit:", err)
		return 1
	}
	return 0
}

// cappedFile writes the first max bytes to a file and counts the rest.
type cappedFile struct {
	mu        sync.Mutex
	f         *os.File
	max       int64
	written   int64
	n         int64
	truncated bool
}

func newCappedFile(path string, max int64) (*cappedFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	return &cappedFile{f: f, max: max}, nil
}

func (c *cappedFile) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n += int64(len(p))
	room := c.max - c.written
	q := p
	if int64(len(q)) > room {
		if room < 0 {
			room = 0
		}
		q = q[:room]
		c.truncated = true
	}
	if len(q) > 0 {
		w, err := c.f.Write(q)
		c.written += int64(w)
		if err != nil {
			// A full disk must not end the command: go on counting.
			c.truncated = true
		}
	}
	return len(p), nil
}

func (c *cappedFile) Count() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *cappedFile) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.truncated
}

func (c *cappedFile) Sync() error  { return c.f.Sync() }
func (c *cappedFile) Close() error { return c.f.Close() }
