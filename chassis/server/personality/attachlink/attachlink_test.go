package attachlink

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/attached"
	"github.com/loremlabs/thanks-computer/chassis/attached/home"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// rig is a Controller over a scratch attach folder, saying its lines into
// said and never signalling the test process.
type rig struct {
	c     *Controller
	h     home.Home
	mu    sync.Mutex
	said  []string
	idled bool
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{h: home.Home{Dir: t.TempDir()}}
	pu := &processor.Unit{Conf: config.Config{Personalities: "attachlink", AttachHome: r.h.Dir}, Logger: zap.NewNop()}
	r.c = NewController(context.Background(), pu)
	r.c.Say = func(line string) { r.mu.Lock(); r.said = append(r.said, line); r.mu.Unlock() }
	r.c.Idle = func() { r.mu.Lock(); r.idled = true; r.mu.Unlock() }
	return r
}

func (r *rig) lines() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.said, "\n")
}

// cred saves a while-running credential for a chassis nobody answers at.
func (r *rig) cred(t *testing.T, id string) home.Credential {
	t.Helper()
	c := home.Credential{ID: id, Address: "http://127.0.0.1:1", Label: "the pony " + id[len(id)-4:], Root: t.TempDir(),
		Seed: make([]byte, ed25519.SeedSize), CreatedAt: time.Now().UTC(), WhileRunning: true}
	if err := r.h.Save(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// job is a detached job of cred's whose end is owed.
func (r *rig) job(t *testing.T, id, cred string) string {
	t.Helper()
	dir := filepath.Join(r.h.JobsDir(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(pending{Credential: cred, Request: "req", OPC: "opc_1", Token: "tok"})
	if err := os.WriteFile(filepath.Join(dir, replyFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func outcome(t *testing.T, dir string) string {
	t.Helper()
	var d struct{ Outcome string }
	raw, err := os.ReadFile(filepath.Join(dir, doneFile))
	if err != nil || json.Unmarshal(raw, &d) != nil {
		return ""
	}
	return d.Outcome
}

const (
	idOther = "atc-0000000000000000000000000000beef"
	idOwn   = "atc-00000000000000000000000000000a0e"
)

// A while-running share whose txco attach died is cleared at the next start:
// its jobs are stopped and its credential removed. One another txco attach
// holds is left alone.
func TestStartClearsLeft(t *testing.T) {
	r := newRig(t)
	left := r.cred(t, "atc-0000000000000000000000000000dead")
	r.cred(t, idOther)
	jl := r.job(t, "job_00000000000000000000000000000001", left.ID)
	jo := r.job(t, "job_00000000000000000000000000000002", idOther)
	f, err := os.OpenFile(filepath.Join(r.h.CredentialsDir(), idOther+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	r.c.Start()
	defer r.c.Stop()

	if _, ok := r.h.Get(left.ID); ok {
		t.Error("the left share was kept")
	}
	if _, err := os.Stat(filepath.Join(jl, "stop")); err != nil {
		t.Errorf("its job was not stopped: %v", err)
	}
	if got := outcome(t, jl); got != "share ended" {
		t.Errorf("its job: %q", got)
	}
	if _, ok := r.h.Get(idOther); !ok {
		t.Error("another txco attach's share was removed")
	}
	if _, err := os.Stat(filepath.Join(jo, "stop")); err == nil || outcome(t, jo) != "" {
		t.Error("another txco attach's job was touched")
	}
	if !strings.Contains(r.lines(), "detached: the pony dead in ") || strings.Contains(r.lines(), "beef") {
		t.Errorf("said:\n%s", r.lines())
	}
	r.mu.Lock()
	idled := r.idled
	r.mu.Unlock()
	if !idled {
		t.Error("nothing left to serve, yet it did not end")
	}
}

// Stopping ends this process's own share: no request is taken, its jobs are
// stopped, and its credential is removed.
func TestStopEndsOwn(t *testing.T) {
	r := newRig(t)
	own := r.cred(t, idOwn)
	if err := r.h.Hold(own.ID); err != nil {
		t.Fatal(err)
	}
	dir := r.job(t, "job_00000000000000000000000000000003", own.ID)

	r.c.Start()
	done := make(chan struct{})
	go func() { r.c.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(endWait + 5*time.Second):
		t.Fatal("Stop did not return")
	}

	if _, ok := r.h.Get(own.ID); ok || r.h.Holds(own.ID) {
		t.Error("the share outlived its txco attach")
	}
	if _, err := os.Stat(filepath.Join(dir, "stop")); err != nil {
		t.Errorf("its job was not stopped: %v", err)
	}
	if got := outcome(t, dir); got != "share ended" {
		t.Errorf("its job: %q", got)
	}
	if !strings.Contains(r.lines(), "detached: the pony 0a0e in ") {
		t.Errorf("said:\n%s", r.lines())
	}
	rep := (&handler{c: r.c, cred: own}).Request(context.Background(), attached.Header{ID: "r1"})
	if rep.Error == nil || rep.Error.Code != "unavailable" {
		t.Errorf("a request while stopping: %+v", rep)
	}
}
