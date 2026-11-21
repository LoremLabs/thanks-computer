package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
	"github.com/loremlabs/thanks-computer/chassis/workspace/detach"
)

// The provider starts a job's wrapper as its own binary's hidden command,
// so in a test the test binary answers to it, as txco does (chassis/app).
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == detach.HiddenCommand {
		os.Exit(detach.RunJob(os.Args[2]))
	}
	os.Exit(m.Run())
}

func TestExecDetached(t *testing.T) {
	p, c, h := newComputer(t)
	d, ok := c.(workspace.Detacher)
	if !ok {
		t.Fatal("the local computer does not detach")
	}
	if err := os.MkdirAll(filepath.Join(h.Ref, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	job, err := d.ExecDetached(context.Background(), workspace.ExecRequest{
		Command: `cat; echo "$WHO in $(basename "$(pwd)") home=$HOME"; exit 4`,
		Stdin:   []byte("fed\n"),
		Cwd:     "sub",
		Env:     map[string]string{"WHO": "pony"},
		Timeout: time.Minute,
	}, workspace.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := p.Jobs()
	if err != nil {
		t.Fatal(err)
	}
	stop := time.Now().Add(10 * time.Second)
	for {
		state, _, err := jobs.Status(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state == detach.Ended {
			break
		}
		if state == detach.Lost || time.Now().After(stop) {
			t.Fatalf("job is %s", state)
		}
		time.Sleep(20 * time.Millisecond)
	}
	res, _, err := jobs.Result(job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The same scrubbed environment and the same cwd rules as a held exec.
	want := "fed\npony in sub home=" + h.Ref + "\n"
	if string(res.Stdout) != want || res.Exit != 4 {
		t.Fatalf("exit %d, stdout %q, want 4 and %q", res.Exit, res.Stdout, want)
	}
	// The jobs live beside the workspaces, where no workspace can be named.
	if got := filepath.Dir(jobs.Root()); got != p.Root() || filepath.Base(jobs.Root()) != jobsDir {
		t.Fatalf("jobs root = %s", jobs.Root())
	}
}

func TestExecDetachedRefusals(t *testing.T) {
	_, c, _ := newComputer(t)
	d := c.(workspace.Detacher)
	for name, req := range map[string]workspace.ExecRequest{
		"no timeout":  {Command: "true"},
		"a terminal":  {Command: "true", Timeout: time.Minute, TTY: true},
		"a bad cwd":   {Command: "true", Timeout: time.Minute, Cwd: "nowhere"},
		"cwd escapes": {Command: "true", Timeout: time.Minute, Cwd: "../.."},
		"no command":  {Timeout: time.Minute},
	} {
		_, err := d.ExecDetached(context.Background(), req, workspace.Limits{})
		var we *workspace.Error
		if !errors.As(err, &we) || we.Code != "bad_request" {
			t.Errorf("%s: err = %v, want bad_request", name, err)
		}
	}
}
