package detach

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// The test binary stands in for txco twice over:
//
//	<test> __job <dir>                    the wrapper, as `txco __job`
//	<test> __starter <root> <seconds> <shell command>
//	                                      a process that starts one job, prints
//	                                      its id, and then waits to be killed —
//	                                      the part `txco attach` plays
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == HiddenCommand {
		os.Exit(RunJob(os.Args[2]))
	}
	if len(os.Args) >= 5 && os.Args[1] == "__starter" {
		os.Exit(starterMain(os.Args[2], os.Args[3], os.Args[4]))
	}
	os.Exit(m.Run())
}

func starterMain(root, seconds, command string) int {
	secs, _ := strconv.Atoi(seconds)
	s, err := New(root, []string{os.Args[0], HiddenCommand})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	job, err := s.Start(context.Background(), Spec{
		Argv:     []string{"/bin/sh", "-c", command},
		Dir:      root,
		Env:      []string{"PATH=/usr/bin:/bin"},
		Deadline: time.Now().Add(time.Duration(secs) * time.Second),
	}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(job.ID)
	select {} // until killed
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir(), []string{os.Args[0], HiddenCommand})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sh(command string, life time.Duration) Spec {
	return Spec{
		Argv:     []string{"/bin/sh", "-c", command},
		Env:      []string{"PATH=/usr/bin:/bin"},
		Deadline: time.Now().Add(life),
	}
}

// waitEnded polls until the job has an exit, failing the test at the bound.
func waitEnded(t *testing.T, s *Store, id string, bound time.Duration) Exit {
	t.Helper()
	stop := time.Now().Add(bound)
	for {
		state, ex, err := s.Status(id)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		switch state {
		case Ended:
			return *ex
		case Lost:
			t.Fatalf("job %s was lost", id)
		}
		if time.Now().After(stop) {
			t.Fatalf("job %s still %s after %s", id, state, bound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func wsCode(err error) string {
	var we *workspace.Error
	if errors.As(err, &we) {
		return we.Code
	}
	return ""
}

func TestStartRunsToItsExit(t *testing.T) {
	s := testStore(t)
	spec := sh(`cat; echo "to err" >&2; echo "$GREETING from $(pwd)"; exit 3`, time.Minute)
	root, err := filepath.EvalSymlinks(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	spec.Dir = root
	spec.Env = append(spec.Env, "GREETING=hello")
	job, err := s.Start(context.Background(), spec, []byte("from stdin\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(job.ID) {
		t.Fatalf("job id %q", job.ID)
	}
	ex := waitEnded(t, s, job.ID, 10*time.Second)
	if ex.Exit != 3 || ex.TimedOut || ex.Stopped {
		t.Fatalf("exit = %+v", ex)
	}
	res, _, err := s.Result(job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "from stdin\nhello from " + root + "\n"
	if got := string(res.Stdout); got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if string(res.Stderr) != "to err\n" {
		t.Fatalf("stderr = %q", res.Stderr)
	}
	if res.Exit != 3 || res.StdoutTruncated || res.StderrTruncated {
		t.Fatalf("result = %+v", res)
	}
	// The spec holds the environment: only the owner may read it.
	st, err := os.Stat(filepath.Join(job.Dir, specFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("spec mode = %v, %v", st.Mode().Perm(), err)
	}
}

// The property the whole design rests on: the process that started a job is
// killed outright, with its whole process group, and the job still runs to
// its end, writes output after the kill, and leaves its real exit status for
// a later process to read.
func TestJobOutlivesItsStarter(t *testing.T) {
	root := t.TempDir()
	starter := exec.Command(os.Args[0], "__starter", root, "60", `echo before; sleep 2; echo after; exit 7`)
	starter.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := starter.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	starter.Stderr = os.Stderr
	if err := starter.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		_ = starter.Process.Kill()
		t.Fatalf("starter said nothing: %v", err)
	}
	id := strings.TrimSpace(line)

	// SIGKILL to the starter's whole group, as a terminal closing or an
	// impatient owner would deliver it.
	if err := syscall.Kill(-starter.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = starter.Wait()
	killedAt := time.Now()

	// A fresh store on the same directory: the restarted `txco attach`.
	s, err := New(root, []string{os.Args[0], HiddenCommand})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := s.Status(id)
	if err != nil {
		t.Fatal(err)
	}
	if state != Running {
		t.Fatalf("after the starter was killed the job is %s, want running", state)
	}
	ex := waitEnded(t, s, id, 15*time.Second)
	if ex.Exit != 7 || ex.TimedOut || ex.Stopped {
		t.Fatalf("exit = %+v, want the command's own 7", ex)
	}
	if !ex.EndedAt.After(killedAt) {
		t.Fatalf("the job ended at %s, before its starter was killed at %s", ex.EndedAt, killedAt)
	}
	res, _, err := s.Result(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "before\nafter\n" {
		t.Fatalf("stdout = %q: the output written after the kill is missing", res.Stdout)
	}
}

func TestDeadlineEndsTheWholeGroup(t *testing.T) {
	s := testStore(t)
	pidFile := filepath.Join(s.Root(), "grandchild.pid")
	// The command ignores TERM, and so does the grandchild it leaves behind:
	// only the KILL after the grace ends them.
	spec := sh(`trap '' TERM; (trap '' TERM; echo $$ > /dev/null; sleep 60) & echo $! > `+pidFile+`; sleep 60`, 700*time.Millisecond)
	spec.KillGraceMS = 300
	start := time.Now()
	job, err := s.Start(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	ex := waitEnded(t, s, job.ID, 15*time.Second)
	if !ex.TimedOut || ex.Stopped {
		t.Fatalf("exit = %+v, want timed out", ex)
	}
	if ex.Exit != -1 || ex.Signal == "" {
		t.Fatalf("exit = %+v, want a signal's -1", ex)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("the deadline took %s to end the job", took)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	gpid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	for i := 0; i < 100 && alive(gpid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(gpid) {
		_ = syscall.Kill(gpid, syscall.SIGKILL)
		t.Fatalf("the grandchild %d outlived the job's deadline", gpid)
	}
}

func TestStop(t *testing.T) {
	s := testStore(t)
	job, err := s.Start(context.Background(), sh(`echo up; sleep 60`, time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	if state, _, _ := s.Status(job.ID); state != Running {
		t.Fatalf("state = %s", state)
	}
	if err := s.Remove(job.ID); wsCode(err) != "bad_request" {
		t.Fatalf("remove of a running job = %v", err)
	}
	if err := s.Stop(job.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(job.ID); err != nil {
		t.Fatalf("a second stop: %v", err)
	}
	ex := waitEnded(t, s, job.ID, 10*time.Second)
	if !ex.Stopped || ex.TimedOut {
		t.Fatalf("exit = %+v, want stopped", ex)
	}
	res, _, _ := s.Result(job.ID, 0)
	if string(res.Stdout) != "up\n" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if err := s.Remove(job.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Status(job.ID); wsCode(err) != workspace.CodeNotFound {
		t.Fatalf("status of a removed job = %v", err)
	}
}

func TestStartFailureIsNotAJob(t *testing.T) {
	s := testStore(t)
	for name, spec := range map[string]Spec{
		"no such program":   {Argv: []string{"/no/such/program"}, Deadline: time.Now().Add(time.Minute)},
		"no such directory": {Argv: []string{"/bin/sh", "-c", "true"}, Dir: "/no/such/dir", Deadline: time.Now().Add(time.Minute)},
	} {
		if _, err := s.Start(context.Background(), spec, nil); wsCode(err) != "bad_request" {
			t.Fatalf("%s: err = %v, want bad_request", name, err)
		}
	}
	if _, err := s.Start(context.Background(), Spec{Argv: []string{"/bin/true"}}, nil); wsCode(err) != "bad_request" {
		t.Fatalf("no deadline: err = %v", err)
	}
	if _, err := s.Start(context.Background(), Spec{Argv: []string{"/bin/true"}, Deadline: time.Now().Add(-time.Second)}, nil); wsCode(err) != "bad_request" {
		t.Fatalf("a past deadline: err = %v", err)
	}
	ids, err := s.List()
	if err != nil || len(ids) != 0 {
		t.Fatalf("failed starts left jobs behind: %v, %v", ids, err)
	}
}

// A wrapper killed outright leaves no exit. The job reads LOST, never ended
// and never running: nobody can say whether its command finished.
func TestWrapperKilledIsLost(t *testing.T) {
	s := testStore(t)
	job, err := s.Start(context.Background(), sh(`sleep 60`, time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := readStarted(job.Dir)
	if !ok {
		t.Fatal("no started.json")
	}
	t.Cleanup(func() { _ = syscall.Kill(-st.CommandPID, syscall.SIGKILL) })
	if err := syscall.Kill(st.WrapperPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	stop := time.Now().Add(5 * time.Second)
	for {
		state, ex, err := s.Status(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state == Lost {
			break
		}
		if state == Ended {
			t.Fatalf("a killed wrapper recorded an exit: %+v", ex)
		}
		if time.Now().After(stop) {
			t.Fatalf("state = %s after the wrapper was killed", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, err := s.Result(job.ID, 0); wsCode(err) != "bad_request" {
		t.Fatalf("result of a lost job = %v", err)
	}
}

func TestSecondWrapperDoesNothing(t *testing.T) {
	s := testStore(t)
	marker := filepath.Join(s.Root(), "ran")
	job, err := s.Start(context.Background(), sh(`echo x >> `+marker+`; sleep 1`, time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	// While the first holds the lock.
	if out, err := exec.Command(os.Args[0], HiddenCommand, job.Dir).CombinedOutput(); err == nil {
		t.Fatalf("a second wrapper ran beside the first: %s", out)
	}
	waitEnded(t, s, job.ID, 10*time.Second)
	// And after the exit is written. The first may still hold the lock for an
	// instant, so either refusal is right; what matters is the marker below.
	_ = exec.Command(os.Args[0], HiddenCommand, job.Dir).Run()
	raw, _ := os.ReadFile(marker)
	if string(raw) != "x\n" {
		t.Fatalf("the command ran %d times", strings.Count(string(raw), "x"))
	}
}

func TestOutputIsCapped(t *testing.T) {
	s := testStore(t)
	spec := sh(`i=0; while [ $i -lt 200 ]; do echo 0123456789012345678901234567890123456789; i=$((i+1)); done; echo done >&2`, time.Minute)
	spec.MaxLogBytes = 1000
	job, err := s.Start(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	ex := waitEnded(t, s, job.ID, 10*time.Second)
	if ex.Exit != 0 || !ex.StdoutTruncated || ex.StderrTruncated {
		t.Fatalf("exit = %+v", ex)
	}
	if ex.StdoutBytes != 200*41 {
		t.Fatalf("stdout bytes = %d, want every byte counted", ex.StdoutBytes)
	}
	st, _ := os.Stat(filepath.Join(job.Dir, stdoutFile))
	if st.Size() != 1000 {
		t.Fatalf("stdout file = %d bytes, want the cap", st.Size())
	}
	res, _, _ := s.Result(job.ID, 100)
	if len(res.Stdout) != 100 || !res.StdoutTruncated {
		t.Fatalf("result stdout = %d bytes, truncated %v", len(res.Stdout), res.StdoutTruncated)
	}
	if string(res.Stderr) != "done\n" || res.StderrTruncated {
		t.Fatalf("result stderr = %q", res.Stderr)
	}
}

// A wrapper that comes up after its starter gave up runs nothing.
func TestStopBeforeStartRunsNothing(t *testing.T) {
	s := testStore(t)
	id, _ := newID()
	dir := filepath.Join(s.Root(), id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(s.Root(), "ran")
	raw, _ := json.Marshal(sh(`touch `+marker, time.Minute))
	if err := writeAtomic(dir, specFile, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(id); err != nil {
		t.Fatal(err)
	}
	if code := RunJob(dir); code != 0 {
		t.Fatalf("wrapper = %d", code)
	}
	state, ex, err := s.Status(id)
	if err != nil || state != Ended || !ex.Stopped {
		t.Fatalf("state = %s, exit = %+v, err = %v", state, ex, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the command ran after a stop")
	}
}

func TestIDsCannotLeaveTheStore(t *testing.T) {
	s := testStore(t)
	for _, id := range []string{"", "..", "../x", "job_", "job_" + strings.Repeat("g", 32), "job_" + strings.Repeat("a", 31) + "/"} {
		if _, err := s.Dir(id); wsCode(err) != "bad_request" {
			t.Fatalf("Dir(%q) = %v", id, err)
		}
		if _, _, err := s.Status(id); wsCode(err) != "bad_request" {
			t.Fatalf("Status(%q) = %v", id, err)
		}
		if err := s.Stop(id); wsCode(err) != "bad_request" {
			t.Fatalf("Stop(%q) = %v", id, err)
		}
	}
}
