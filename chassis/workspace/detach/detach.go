// Package detach runs a command that must outlive the process that started
// it: `workspace://…/exec WITH detach = true`.
//
// A job is a directory. The starter writes the job's spec there and launches
// a WRAPPER — the txco binary in its hidden `__job` mode — in a session of
// its own. The wrapper runs the command, enforces the job's deadline itself,
// and writes the exit to disk atomically. Nothing ties the wrapper to its
// starter: no pipe, no terminal, no process group. So the starter may be
// killed and restarted, and a later process still reads the job's real exit
// from its directory.
//
//	<root>/<job id>/
//	  spec.json     what to run (mode 0600: it holds the environment)
//	  stdin         the command's stdin, when it has one
//	  lock          held (flock) by the wrapper for as long as it lives
//	  started.json  written once the command has started
//	  stdout        the command's stdout, capped
//	  stderr        the command's stderr, capped
//	  stop          created to ask the wrapper to end the command
//	  exit.json     the outcome, written once, by rename
//	  wrapper.log   the wrapper's own complaints, normally empty
//
// Three rules carry the design:
//
//   - A wrapper is alive exactly while it holds the lock. Liveness is read by
//     trying the lock, never from a pid, so a reused pid is never mistaken for
//     a job.
//   - exit.json is written before the lock is released. A job with no exit and
//     a free lock is LOST (the wrapper was killed, or the machine restarted):
//     whether its command finished cannot be known.
//   - A job is stopped by a file, not a signal, for the same reason liveness
//     is not read from a pid.
package detach

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

const (
	specFile    = "spec.json"
	stdinFile   = "stdin"
	lockFile    = "lock"
	startedFile = "started.json"
	stdoutFile  = "stdout"
	stderrFile  = "stderr"
	stopFile    = "stop"
	exitFile    = "exit.json"
	wrapperLog  = "wrapper.log"
)

// HiddenCommand is the txco subcommand that runs a job's wrapper:
// `txco __job <job directory>`.
const HiddenCommand = "__job"

// DefaultKillGrace is how long a command has between the wrapper's TERM and
// its KILL, at the deadline or on a stop.
const DefaultKillGrace = 10 * time.Second

// DefaultMaxLogBytes caps each of a job's output files. The command keeps
// running past the cap; what it writes after is counted and dropped.
const DefaultMaxLogBytes int64 = 16 << 20

// startWait bounds how long Start waits for the wrapper to report that the
// command started.
const startWait = 10 * time.Second

// Spec is what a job runs. Env is the command's whole environment, already
// resolved by the caller: the wrapper adds nothing to it.
type Spec struct {
	Argv        []string  `json:"argv"`
	Dir         string    `json:"dir"`
	Env         []string  `json:"env"`
	Deadline    time.Time `json:"deadline"`
	KillGraceMS int64     `json:"kill_grace_ms,omitempty"`
	MaxLogBytes int64     `json:"max_log_bytes,omitempty"`
	HasStdin    bool      `json:"has_stdin,omitempty"`
}

// Started is the wrapper's word that the command is running.
type Started struct {
	WrapperPID int       `json:"wrapper_pid"`
	CommandPID int       `json:"command_pid"`
	StartedAt  time.Time `json:"started_at"`
}

// Exit is a job's outcome, as the wrapper recorded it.
type Exit struct {
	// Exit is the command's exit code; -1 when a signal ended it or it never
	// started.
	Exit int `json:"exit"`
	// TimedOut: the wrapper ended the command at its deadline.
	TimedOut bool `json:"timed_out,omitempty"`
	// Stopped: the wrapper ended the command because it was asked to.
	Stopped bool `json:"stopped,omitempty"`
	// StartError: the command could not be started at all.
	StartError string `json:"start_error,omitempty"`
	// Signal names the signal that ended the command, when one did.
	Signal    string    `json:"signal,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	WallMS    int64     `json:"wall_ms"`
	// What the command wrote, and whether the output files' cap dropped any.
	StdoutBytes     int64 `json:"stdout_bytes"`
	StderrBytes     int64 `json:"stderr_bytes"`
	StdoutTruncated bool  `json:"stdout_truncated,omitempty"`
	StderrTruncated bool  `json:"stderr_truncated,omitempty"`
}

// State is where a job stands.
type State string

const (
	// Running: the wrapper holds the job's lock.
	Running State = "running"
	// Ended: exit.json is there.
	Ended State = "ended"
	// Lost: no exit, and no wrapper. Whether the command finished is unknown.
	Lost State = "lost"
)

// Job is a started job.
type Job struct {
	ID        string
	Dir       string
	StartedAt time.Time
}

// Store is a directory of jobs.
type Store struct {
	root    string
	wrapper []string
}

// New returns the store rooted at root (created if needed, mode 0700).
// wrapper is the argv that runs a job's wrapper, the job's directory being
// appended to it; nil means this binary's own hidden command.
func New(root string, wrapper []string) (*Store, error) {
	if root == "" {
		return nil, errors.New("detach: empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("detach: root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("detach: mkdir root: %w", err)
	}
	if len(wrapper) == 0 {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("detach: own executable: %w", err)
		}
		// The real file, not a name it was run by: `thanks` is a symlink to
		// this binary that rewrites its arguments (chassis/app roomAlias).
		if real, err := filepath.EvalSymlinks(self); err == nil {
			self = real
		}
		wrapper = []string{self, HiddenCommand}
	}
	return &Store{root: abs, wrapper: append([]string(nil), wrapper...)}, nil
}

// Root is the store's directory.
func (s *Store) Root() string { return s.root }

var idRE = regexp.MustCompile(`^job_[0-9a-f]{32}$`)

// ValidID reports whether id has a job id's shape. Every method that takes
// an id checks it, so an id can never name a path outside the store.
func ValidID(id string) bool { return idRE.MatchString(id) }

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "job_" + hex.EncodeToString(b[:]), nil
}

// Dir is the job's directory, or an error for a malformed id.
func (s *Store) Dir(id string) (string, error) {
	if !ValidID(id) {
		return "", &workspace.Error{Code: "bad_request", Message: "not a job id"}
	}
	return filepath.Join(s.root, id), nil
}

// Start launches spec as a detached job and returns once its command is
// running. A command that cannot be started (no such program, no such
// directory) is a bad_request, reported here and not as a job.
//
// ctx bounds only the wait for the start; it is never the job's life. The
// job's life is spec.Deadline, which is required.
func (s *Store) Start(ctx context.Context, spec Spec, stdin []byte) (Job, error) {
	if len(spec.Argv) == 0 || spec.Argv[0] == "" {
		return Job{}, &workspace.Error{Code: "bad_request", Message: "no command"}
	}
	if spec.Deadline.IsZero() {
		return Job{}, &workspace.Error{Code: "bad_request", Message: "a detached job needs a deadline"}
	}
	if !spec.Deadline.After(time.Now()) {
		return Job{}, &workspace.Error{Code: "bad_request", Message: "a detached job's deadline is already past"}
	}
	id, err := newID()
	if err != nil {
		return Job{}, &workspace.Error{Code: "provider", Message: "job id: " + err.Error()}
	}
	dir := filepath.Join(s.root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return Job{}, &workspace.Error{Code: "provider", Message: "job dir: " + err.Error()}
	}
	fail := func(code, msg string) (Job, error) {
		_ = os.RemoveAll(dir)
		return Job{}, &workspace.Error{Code: code, Message: msg}
	}
	spec.HasStdin = len(stdin) > 0
	if spec.HasStdin {
		if err := os.WriteFile(filepath.Join(dir, stdinFile), stdin, 0o600); err != nil {
			return fail("provider", "job stdin: "+err.Error())
		}
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return fail("provider", "job spec: "+err.Error())
	}
	if err := writeAtomic(dir, specFile, raw); err != nil {
		return fail("provider", "job spec: "+err.Error())
	}

	logf, err := os.OpenFile(filepath.Join(dir, wrapperLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fail("provider", "job log: "+err.Error())
	}
	argv := append(append([]string(nil), s.wrapper...), dir)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	// stdin is /dev/null (os/exec's nil), and both outputs are a FILE: the
	// wrapper holds no pipe to this process, so this process dying cannot
	// reach it.
	cmd.Stdout = logf
	cmd.Stderr = logf
	// A session of its own: no controlling terminal, and neither this
	// process's group nor its death signals the wrapper.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	startErr := cmd.Start()
	_ = logf.Close()
	if startErr != nil {
		return fail("provider", "start the job's wrapper: "+startErr.Error())
	}
	exited := make(chan struct{})
	go func() {
		// Reap the wrapper if it ends while this process still lives, so it
		// never lingers as a zombie.
		_ = cmd.Wait()
		close(exited)
	}()

	deadline := time.NewTimer(startWait)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	giveUp := func(why string) (Job, error) {
		// The wrapper may yet come up. The stop file makes sure that if it
		// does, it runs nothing. The directory is left for it to find.
		_ = s.Stop(id)
		return Job{}, &workspace.Error{Code: "provider", Message: why}
	}
	wrapperGone := false
	for {
		if st, ok := readStarted(dir); ok {
			return Job{ID: id, Dir: dir, StartedAt: st.StartedAt}, nil
		}
		if ex, ok := readExit(dir); ok {
			if ex.StartError != "" {
				return fail("bad_request", ex.StartError)
			}
			// It ran and ended before we looked.
			return Job{ID: id, Dir: dir, StartedAt: ex.StartedAt}, nil
		}
		if wrapperGone {
			return fail("provider", "the job's wrapper exited before starting the command"+logTail(dir))
		}
		select {
		case <-exited:
			// One more look: it may have written its files just before exiting.
			wrapperGone = true
		case <-tick.C:
		case <-deadline.C:
			return giveUp("the job's wrapper did not report a start")
		case <-ctx.Done():
			return giveUp("gave up waiting for the job to start: " + ctx.Err().Error())
		}
	}
}

func logTail(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, wrapperLog))
	if err != nil || len(b) == 0 {
		return ""
	}
	if len(b) > 500 {
		b = b[len(b)-500:]
	}
	return ": " + string(b)
}

// Stop asks a job's wrapper to end its command: TERM to the command's group,
// then KILL after the job's grace. The exit then says stopped. Asking twice,
// or asking of a job that has ended, changes nothing.
func (s *Store) Stop(id string) error {
	dir, err := s.Dir(id)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, stopFile), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsNotExist(err) {
			return &workspace.Error{Code: workspace.CodeNotFound, Message: "no such job"}
		}
		return &workspace.Error{Code: "provider", Message: "stop: " + err.Error()}
	}
	return f.Close()
}

// Status says where a job stands, with its exit once it has ended.
func (s *Store) Status(id string) (State, *Exit, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return "", nil, err
	}
	if _, err := os.Stat(dir); err != nil {
		return "", nil, &workspace.Error{Code: workspace.CodeNotFound, Message: "no such job"}
	}
	if ex, ok := readExit(dir); ok {
		return Ended, &ex, nil
	}
	held, err := lockHeld(dir)
	if err != nil {
		return "", nil, &workspace.Error{Code: "provider", Message: "job lock: " + err.Error()}
	}
	if held {
		return Running, nil, nil
	}
	// The wrapper writes the exit BEFORE it lets go of the lock, so a free
	// lock with an exit means it ended between our two looks.
	if ex, ok := readExit(dir); ok {
		return Ended, &ex, nil
	}
	return Lost, nil, nil
}

// Result is an ended job's outcome in an exec's shape: the exit, and the
// first max bytes of each output (max <= 0: the workspace default).
func (s *Store) Result(id string, max int64) (workspace.ExecResult, Exit, error) {
	dir, err := s.Dir(id)
	if err != nil {
		return workspace.ExecResult{Exit: -1}, Exit{}, err
	}
	ex, ok := readExit(dir)
	if !ok {
		return workspace.ExecResult{Exit: -1}, Exit{}, &workspace.Error{Code: "bad_request", Message: "the job has not ended"}
	}
	if max <= 0 {
		max = workspace.DefaultMaxOutputBytes
	}
	out, outCut := readCapped(filepath.Join(dir, stdoutFile), max)
	errb, errCut := readCapped(filepath.Join(dir, stderrFile), max)
	return workspace.ExecResult{
		Exit:            ex.Exit,
		Stdout:          out,
		Stderr:          errb,
		StdoutTruncated: outCut || ex.StdoutTruncated,
		StderrTruncated: errCut || ex.StderrTruncated,
		StdoutBytes:     ex.StdoutBytes,
		WallMS:          ex.WallMS,
	}, ex, nil
}

func readCapped(path string, max int64) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, max))
	st, err := f.Stat()
	return b, err == nil && st.Size() > max
}

// List returns the ids of every job in the store, oldest name first.
func (s *Store) List() ([]string, error) {
	ents, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() && ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Remove deletes a job's directory. A running job is refused: its wrapper
// would go on writing into nothing.
func (s *Store) Remove(id string) error {
	dir, err := s.Dir(id)
	if err != nil {
		return err
	}
	// Once the exit is written the wrapper has nothing left to write, even
	// if it has not yet let go of the lock.
	if _, ended := readExit(dir); !ended {
		if held, _ := lockHeld(dir); held {
			return &workspace.Error{Code: "bad_request", Message: "the job is still running"}
		}
	}
	return os.RemoveAll(dir)
}

func readStarted(dir string) (Started, bool) {
	var st Started
	b, err := os.ReadFile(filepath.Join(dir, startedFile))
	if err != nil || json.Unmarshal(b, &st) != nil {
		return Started{}, false
	}
	return st, true
}

func readExit(dir string) (Exit, bool) {
	var ex Exit
	b, err := os.ReadFile(filepath.Join(dir, exitFile))
	if err != nil || json.Unmarshal(b, &ex) != nil {
		return Exit{}, false
	}
	return ex, true
}

// lockHeld reports whether a wrapper holds the job's lock. No lock file
// means no wrapper ever took it.
func lockHeld(dir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

// writeAtomic writes name in dir so that a reader sees the old content or
// the whole new content, never a part: a temporary file, synced, renamed
// over the name, and the directory synced.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		cleanup()
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
