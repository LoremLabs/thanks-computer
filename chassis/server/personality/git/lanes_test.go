package git

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/gitstore"
)

// fakeStack answers every envelope on the bus the way a tenant's `_git`
// stack would, and remembers what it saw.
type fakeStack struct {
	mu      sync.Mutex
	seen    []string
	who     []string
	respond func(raw string) string
}

func (f *fakeStack) serve(bus <-chan *event.Envelope) {
	go func() {
		for env := range bus {
			if env == nil {
				return
			}
			raw := env.Payload.Raw
			f.mu.Lock()
			f.seen = append(f.seen, raw)
			a, _ := authn.AuthenticatedFrom(env.Ctx)
			f.who = append(f.who, a.Principal.ID)
			f.mu.Unlock()
			out := "{}"
			if f.respond != nil {
				out = f.respond(raw)
			}
			go func(env *event.Envelope, out string) { env.ResCh <- event.Payload{Raw: out, Type: event.JSON} }(env, out)
		}
	}()
}

func (f *fakeStack) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		got := append([]string{}, f.seen...)
		f.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("saw %d envelopes, want %d", len(got), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (d *doorHarness) withStack(t *testing.T, fs *fakeStack) {
	t.Helper()
	bus := make(chan *event.Envelope, 16)
	d.ctrl.pu.Bus = bus
	d.ctrl.lanes.subscribed = func(tenant string) bool { return tenant == "acme" }
	d.ctrl.lanes.deadline = 500 * time.Millisecond
	fs.serve(bus)
}

func TestLanesAnswerAndObserve(t *testing.T) {
	gitPath(t)
	d := newDoorHarness(t)
	ctx := context.Background()
	if _, _, err := d.repos.Create(ctx, gitstore.Repo{Tenant: "acme", Name: "pony", Principal: "pony:pony", CreatedBy: "ranch"}); err != nil {
		t.Fatal(err)
	}
	d.grant(t, "pony:pony", "pony@pony.example.com", "bcdf-correct-horse", "git:pony:fetch", "git:pony:push")
	fs := &fakeStack{}
	// The stack accepts, unless a file outside config/ is added.
	fs.respond = func(raw string) string {
		if gjson.Get(raw, "_txc.git.phase").String() != "answer" {
			return "{}"
		}
		for _, ch := range gjson.Get(raw, "_txc.git.changes").Array() {
			if p := ch.Get("path").String(); !strings.HasPrefix(p, "config/") && p != "README.md" {
				return `{"_txc":{"git":{"res":{"ok":false,"code":"cannot","msg":"only config/ and README.md may change: ` + p + `"}}}}`
			}
		}
		return `{"_txc":{"git":{"res":{"ok":true}}}}`
	}
	d.withStack(t, fs)

	work := t.TempDir()
	owner := d.authURL("pony", "pony@pony.example.com", "bcdf-correct-horse")
	d.must(work, "clone", owner, "a")
	a := filepath.Join(work, "a")
	write(t, a, "config/MISSION.md", "# Mission\n")
	d.must(a, "add", ".")
	d.must(a, "commit", "-qm", "first")
	d.must(a, "push", "-q", "origin", "main")

	// One answer envelope before the refs moved, one observe after the reply.
	seen := fs.wait(t, 2)
	ans, obs := seen[0], seen[1]
	if gjson.Get(ans, "_txc.git.phase").String() != "answer" || gjson.Get(obs, "_txc.git.phase").String() != "observe" {
		t.Fatalf("phases: %s / %s", gjson.Get(ans, "_txc.git.phase"), gjson.Get(obs, "_txc.git.phase"))
	}
	for _, raw := range seen {
		if gjson.Get(raw, "_txc.src").String() != "git" || gjson.Get(raw, "_txc.git.tenant").String() != "acme" ||
			gjson.Get(raw, "_txc.git.repo").String() != "pony" || gjson.Get(raw, "_txc.git.ref").String() != "refs/heads/main" ||
			gjson.Get(raw, "_txc.git.op").String() != "create" || gjson.Get(raw, "_txc.git.pusher.principal").String() != "pony:pony" ||
			gjson.Get(raw, "_txc.git.pusher.credential").String() == "" {
			t.Fatalf("envelope facts: %s", raw)
		}
		if ch := gjson.Get(raw, "_txc.git.changes").Array(); len(ch) != 1 || ch[0].Get("path").String() != "config/MISSION.md" || ch[0].Get("op").String() != "add" {
			t.Fatalf("changes: %s", gjson.Get(raw, "_txc.git.changes"))
		}
		if cs := gjson.Get(raw, "_txc.git.commits").Array(); len(cs) != 1 || cs[0].Get("message").String() != "first\n" || gjson.Get(raw, "_txc.git.commits_total").Int() != 1 {
			t.Fatalf("commits: %s", gjson.Get(raw, "_txc.git.commits"))
		}
		if gjson.Get(raw, "_txc.git.old").String() != "0000000000000000000000000000000000000000" {
			t.Fatalf("old: %s", gjson.Get(raw, "_txc.git.old"))
		}
	}
	fs.mu.Lock()
	who := fs.who[0]
	fs.mu.Unlock()
	if who != "pony:pony" {
		t.Fatalf("the run acts as %q, want the pusher", who)
	}

	// The stack declines: the refs stay, the developer reads the sentence,
	// and nothing is observed.
	write(t, a, "notes/private.md", "mine\n")
	d.must(a, "add", ".")
	d.must(a, "commit", "-qm", "outside")
	out := d.fails(a, "only config/ and README.md may change: notes/private.md", "push", "origin", "main")
	_ = out
	time.Sleep(100 * time.Millisecond)
	if n := len(fs.wait(t, 3)); n != 3 {
		t.Fatalf("envelopes after a refusal: %d, want 3 (no observe)", n)
	}
	if got := strings.TrimSpace(d.must(a, "rev-list", "--count", "origin/main")); got != "1" {
		t.Fatalf("origin/main moved despite the refusal: %s", got)
	}
	// The third envelope was the answer for the second push; no observe followed.
	fs.mu.Lock()
	third := fs.seen[2]
	fs.mu.Unlock()
	if gjson.Get(third, "_txc.git.phase").String() != "answer" {
		t.Fatalf("third envelope: %s", gjson.Get(third, "_txc.git.phase"))
	}
}

// seededClone makes the repository, grants the owner and returns a clone
// with one unpushed commit.
func (d *doorHarness) seededClone(t *testing.T, policy string) string {
	t.Helper()
	ctx := context.Background()
	if _, _, err := d.repos.Create(ctx, gitstore.Repo{Tenant: "acme", Name: "pony", Principal: "pony:pony", PolicyPush: policy}); err != nil {
		t.Fatal(err)
	}
	d.grant(t, "pony:pony", "pony@pony.example.com", "bcdf-correct-horse", "git:pony:fetch", "git:pony:push")
	work := t.TempDir()
	d.must(work, "clone", d.authURL("pony", "pony@pony.example.com", "bcdf-correct-horse"), "a")
	a := filepath.Join(work, "a")
	write(t, a, "config/MISSION.md", "# Mission\n")
	d.must(a, "add", ".")
	d.must(a, "commit", "-qm", "first")
	return a
}

func TestLanesDefaultDeny(t *testing.T) {
	gitPath(t)
	terse := func(raw string) string {
		if gjson.Get(raw, "_txc.git.phase").String() == "answer" {
			return `{"_txc":{"git":{"res":{"ok":false,"code":"limit"}}}}`
		}
		return "{}"
	}
	for _, tc := range []struct {
		name    string
		respond func(string) string // nil: no `_git` stack at all
		want    string
	}{
		{"no stack", nil, "no _git stack to decide this push"},
		{"a stack that never answers", func(string) string { return "{}" }, "declined by the repository's stack"},
		{"a stack that refuses without a sentence", terse, "declined by the repository's stack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDoorHarness(t)
			if tc.respond == nil {
				d.ctrl.lanes.subscribed = func(string) bool { return false }
			} else {
				d.withStack(t, &fakeStack{respond: tc.respond})
			}
			a := d.seededClone(t, "stack")
			d.fails(a, tc.want, "push", "origin", "main")
			if got := strings.TrimSpace(d.must(a, "ls-remote", "--heads", "origin")); got != "" {
				t.Fatalf("a ref moved despite the refusal: %q", got)
			}
		})
	}
	t.Run("policy open: no lane", func(t *testing.T) {
		d := newDoorHarness(t)
		d.ctrl.lanes.subscribed = func(string) bool { return false }
		a := d.seededClone(t, "open")
		d.must(a, "push", "-q", "origin", "main")
	})
}

func TestTranslateAnswer(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		ok      bool
		code    string
		msg     string
		outcome string
	}{
		{`{}`, false, "", "declined by the repository's stack", "absent"},
		{`{"_txc":{"git":{"res":{"ok":true}}}}`, true, "", "", "ok"},
		{`{"_txc":{"git":{"res":{"ok":"true"}}}}`, false, "", "declined by the repository's stack", "refused"},
		{`{"_txc":{"git":{"res":{"ok":false,"code":"cannot","msg":"MISSION.md is required"}}}}`, false, "cannot", "MISSION.md is required", "refused"},
		{`{"_txc":{"git":{"res":{"ok":false,"code":"weird"}}}}`, false, "", "declined by the repository's stack", "refused"},
	} {
		a := translateAnswer(tc.raw)
		if a.ok != tc.ok || a.code != tc.code || a.msg != tc.msg || a.outcome != tc.outcome {
			t.Errorf("translateAnswer(%s) = %+v", tc.raw, a)
		}
	}
}
