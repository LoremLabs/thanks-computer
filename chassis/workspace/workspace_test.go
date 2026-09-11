package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in         string
		name, verb string
		ok         bool
	}{
		{"workspace://tools/exec", "tools", "exec", true},
		{"workspace://pony/paris/exec", "pony/paris", "exec", true},
		{"workspace://a/b/c/d/destroy", "a/b/c/d", "destroy", true},
		{"workspace://tools", "", "", false},            // no verb
		{"workspace://tools/", "", "", false},           // empty verb
		{"workspace:///exec", "", "", false},            // empty name
		{"workspace://tools/frobnicate", "", "", false}, // unknown verb
		{"compute://sha256/abc", "", "", false},         // wrong scheme
		{"", "", "", false},
	}
	for _, c := range cases {
		name, verb, ok := ParseRef(c.in)
		if ok != c.ok || name != c.name || verb != c.verb {
			t.Errorf("ParseRef(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, name, verb, ok, c.name, c.verb, c.ok)
		}
	}
}

func TestValidateName(t *testing.T) {
	good := []string{"tools", "pony/paris", "a/b/c/d", "x1-y2", "0"}
	for _, n := range good {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	bad := map[string]string{
		"":                      "empty",
		"..":                    "must match",
		"a--b":                  "--",
		"exec":                  "verb",
		"pony/create":           "verb",
		"a/b/c/d/e":             "segments",
		"Tools":                 "must match",
		"-a":                    "must match",
		"a/":                    "must match",
		"/a":                    "must match",
		"a b":                   "must match",
		strings.Repeat("a", 64): "must match",
	}
	for n, want := range bad {
		err := ValidateName(n)
		if err == nil {
			t.Errorf("ValidateName(%q) = nil, want error containing %q", n, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateName(%q) = %v, want error containing %q", n, err, want)
		}
	}
}

func TestProviderName(t *testing.T) {
	a := ProviderName("acme", "agents", "pony/paris")
	if !strings.HasPrefix(a, "acme-agents-pony-paris-") {
		t.Errorf("ProviderName prefix = %q", a)
	}
	if len(a) > 49 {
		t.Errorf("ProviderName too long: %d", len(a))
	}
	// Distinct identities never collide, even when the readable prefix is
	// truncated to the same 40 characters.
	long1 := ProviderName("tenant-with-a-very-long-slug", "stack-with-long-name", "workspace-one")
	long2 := ProviderName("tenant-with-a-very-long-slug", "stack-with-long-name", "workspace-two")
	if long1 == long2 {
		t.Errorf("truncated identities collide: %q", long1)
	}
	if len(long1) > 49 || len(long2) > 49 {
		t.Errorf("truncated names too long: %d %d", len(long1), len(long2))
	}
	if strings.Contains(long1, "--") {
		t.Errorf("truncation left a double hyphen: %q", long1)
	}
	first, second := ProviderName("a", "b", "c"), ProviderName("a", "b", "c")
	if first != second {
		t.Errorf("ProviderName not deterministic: %q vs %q", first, second)
	}
}

func TestSortedEnvAndCappedWriter(t *testing.T) {
	env := SortedEnv(map[string]string{"B": "2", "A": "1"})
	if len(env) != 2 || env[0] != "A=1" || env[1] != "B=2" {
		t.Errorf("SortedEnv = %v", env)
	}
	w := NewCappedWriter(5)
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatalf("write: %d %v", n, err)
	}
	if n, err := w.Write([]byte("defgh")); n != 5 || err != nil {
		t.Fatalf("write: %d %v", n, err)
	}
	if string(w.Bytes()) != "abcde" || !w.Truncated() {
		t.Errorf("capped = %q truncated=%v", w.Bytes(), w.Truncated())
	}
	if NewCappedWriter(0).max != DefaultMaxOutputBytes {
		t.Error("zero cap not defaulted")
	}
}

// fakeProvider counts lifecycle calls so the Manager's caching, forget-on-
// failure and run-id behavior can be asserted without a real backend.
type fakeProvider struct {
	created, woken, destroyed int
	wakeErr                   error
	vanishOnce                bool // the next Wake reports not_found, then clears
	execErr                   error
	warm                      time.Duration // WarmThreshold when > 0
	newRun                    bool          // what the woken computer reports
	checkpoints               int
}

func (f *fakeProvider) Name() string                          { return "fake" }
func (f *fakeProvider) Capabilities() []string                { return []string{"exec", "checkpoint"} }
func (f *fakeProvider) Sleep(context.Context, Handle) error   { return nil }
func (f *fakeProvider) WarmThreshold() time.Duration          { return f.warm }
func (f *fakeProvider) NewRun() bool                          { return f.newRun }
func (f *fakeProvider) Destroy(context.Context, Handle) error { f.destroyed++; return nil }
func (f *fakeProvider) Status(context.Context, Handle) (Status, error) {
	return Status{State: "running"}, nil
}
func (f *fakeProvider) Create(_ context.Context, spec Spec) (Handle, error) {
	f.created++
	return Handle{Provider: "fake", Ref: "ref/" + spec.Name, Name: spec.Name}, nil
}
func (f *fakeProvider) Wake(context.Context, Handle) (Computer, error) {
	f.woken++
	if f.vanishOnce {
		f.vanishOnce = false
		return nil, &Error{Code: CodeNotFound, Message: "gone"}
	}
	if f.wakeErr != nil {
		return nil, f.wakeErr
	}
	return f, nil
}
func (f *fakeProvider) Exec(_ context.Context, req ExecRequest, lim Limits) (ExecResult, error) {
	if f.execErr != nil {
		return ExecResult{Exit: -1}, f.execErr
	}
	return ExecResult{Exit: 0, Stdout: []byte(req.Command)}, nil
}
func (f *fakeProvider) Checkpoint(_ context.Context, h Handle, comment string) (string, error) {
	f.checkpoints++
	return "v1", nil
}

func TestManagerCachesHandleAndMintsRunPerExec(t *testing.T) {
	f := &fakeProvider{}
	m := NewManager(f, Limits{}, nil)
	spec := Spec{Tenant: "acme", Stack: "agents", Name: "tools"}

	res, h, run1, err := m.Exec(context.Background(), spec, ExecRequest{Command: "one"})
	if err != nil || string(res.Stdout) != "one" || h.Ref != "ref/tools" || run1 == "" {
		t.Fatalf("first exec: res=%+v h=%+v run=%q err=%v", res, h, run1, err)
	}
	_, _, run2, err := m.Exec(context.Background(), spec, ExecRequest{Command: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if f.created != 1 {
		t.Errorf("Create called %d times, want 1 (cached handle)", f.created)
	}
	if f.woken != 2 {
		t.Errorf("Wake called %d times, want 2", f.woken)
	}
	if run1 == run2 {
		t.Errorf("run id reused across execs on a provider with no warm window: %q", run1)
	}
	if m.lim.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Errorf("zero MaxOutputBytes not defaulted: %d", m.lim.MaxOutputBytes)
	}
}

func TestManagerRunIDPolicyWithWarmWindow(t *testing.T) {
	f := &fakeProvider{warm: 30 * time.Second}
	m := NewManager(f, Limits{}, nil)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	spec := Spec{Tenant: "acme", Stack: "agents", Name: "tools"}

	_, _, run1, _ := m.Exec(context.Background(), spec, ExecRequest{Command: "x"})
	now = now.Add(10 * time.Second)
	_, _, run2, _ := m.Exec(context.Background(), spec, ExecRequest{Command: "x"})
	if run1 != run2 {
		t.Errorf("inside the warm window the run id changed: %q → %q", run1, run2)
	}
	now = now.Add(31 * time.Second)
	_, _, run3, _ := m.Exec(context.Background(), spec, ExecRequest{Command: "x"})
	if run3 == run2 {
		t.Errorf("past the warm window the run id was reused: %q", run3)
	}
	// Inside the window but the computer reports a fresh start.
	now = now.Add(time.Second)
	f.newRun = true
	_, _, run4, _ := m.Exec(context.Background(), spec, ExecRequest{Command: "x"})
	if run4 == run3 {
		t.Errorf("RunReporter.NewRun() ignored: %q", run4)
	}
}

func TestManagerForgetsOnWakeFailureAndDestroy(t *testing.T) {
	f := &fakeProvider{wakeErr: errors.New("gone")}
	m := NewManager(f, Limits{MaxOutputBytes: 10}, nil)
	spec := Spec{Tenant: "acme", Stack: "agents", Name: "tools"}

	if _, _, _, err := m.Exec(context.Background(), spec, ExecRequest{Command: "x"}); err == nil {
		t.Fatal("wake failure not surfaced")
	}
	f.wakeErr = nil
	if _, _, _, err := m.Exec(context.Background(), spec, ExecRequest{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	if f.created != 2 {
		t.Errorf("Create called %d times, want 2 (handle forgotten after wake failure)", f.created)
	}
	if err := m.Destroy(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if f.destroyed != 1 {
		t.Errorf("Destroy called %d times, want 1", f.destroyed)
	}
	if _, _, _, err := m.Exec(context.Background(), spec, ExecRequest{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	if f.created != 3 {
		t.Errorf("Create called %d times, want 3 (recreated after destroy)", f.created)
	}
}

func TestManagerWithStore(t *testing.T) {
	s := newTestStore(t)
	f := &fakeProvider{warm: 30 * time.Second}
	m := NewManager(f, Limits{}, s)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	s.now = m.now
	spec := Spec{Tenant: "acme", Stack: "agents", Name: "tools", Network: "none"}
	ctx := context.Background()

	_, h, run1, err := m.Exec(ctx, spec, ExecRequest{Command: "x"})
	if err != nil {
		t.Fatal(err)
	}
	row, _ := s.Get(ctx, "acme", "agents", "tools")
	if row == nil || row.ProviderRef != h.Ref || row.Provider != "fake" || row.RunID != run1 || row.Status != StatusRunning || row.Network != "none" || !row.LastUsedAt.Equal(now) {
		t.Fatalf("row after exec = %+v", row)
	}

	// A fresh Manager (restart) finds the identity in the table: no Create,
	// and the run id continues inside the warm window.
	m2 := NewManager(f, Limits{}, s)
	now = now.Add(5 * time.Second)
	m2.now = func() time.Time { return now }
	_, h2, run2, err := m2.Exec(ctx, spec, ExecRequest{Command: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if f.created != 1 || h2.Ref != h.Ref {
		t.Errorf("restart recreated: created=%d ref=%q", f.created, h2.Ref)
	}
	if run2 != run1 {
		t.Errorf("run id not continued from the table inside the warm window: %q vs %q", run2, run1)
	}

	// Checkpoint records the ref.
	if ref, _, err := m2.Checkpoint(ctx, spec, "after install"); err != nil || ref != "v1" {
		t.Fatalf("Checkpoint = %q, %v", ref, err)
	}
	row, _ = s.Get(ctx, "acme", "agents", "tools")
	if row.CheckpointRef != "v1" {
		t.Errorf("checkpoint_ref = %q", row.CheckpointRef)
	}

	// Destroy marks the row; the next exec recreates and rewrites it.
	if err := m2.Destroy(ctx, spec); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get(ctx, "acme", "agents", "tools")
	if row.Status != StatusDestroyed {
		t.Errorf("after destroy = %+v", row)
	}
	m3 := NewManager(f, Limits{}, s)
	if _, _, _, err := m3.Exec(ctx, spec, ExecRequest{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	row, _ = s.Get(ctx, "acme", "agents", "tools")
	if f.created != 2 || row.Status != StatusRunning || row.DestroyedAt != nil || row.CheckpointRef != "" {
		t.Errorf("after recreate: created=%d row=%+v", f.created, row)
	}

	// A row recorded by a different provider is not trusted: recreate.
	if err := s.Upsert(ctx, Row{Tenant: "acme", Stack: "agents", Name: "other", Provider: "sprites", ProviderRef: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, h4, _, err := m3.Exec(ctx, Spec{Tenant: "acme", Stack: "agents", Name: "other"}, ExecRequest{Command: "x"}); err != nil || h4.Ref != "ref/other" {
		t.Errorf("foreign-provider row reused: %+v %v", h4, err)
	}
	row, _ = s.Get(ctx, "acme", "agents", "other")
	if row.Provider != "fake" {
		t.Errorf("row not rewritten for this provider: %+v", row)
	}
}

func TestManagerCheckpointUnsupported(t *testing.T) {
	type plain struct{ *fakeProvider }
	// A provider that does not implement Checkpointer.
	var p Provider = struct{ Provider }{&fakeProvider{}}
	_ = plain{}
	m := NewManager(p, Limits{}, nil)
	_, _, err := m.Checkpoint(context.Background(), Spec{Tenant: "t", Stack: "s", Name: "n"}, "")
	var we *Error
	if !errors.As(err, &we) || we.Code != "unsupported" {
		t.Errorf("Checkpoint on a plain provider: %v", err)
	}
}

func TestOpenUnknownProvider(t *testing.T) {
	if _, err := Open("no-such-provider", Config{}); err == nil {
		t.Fatal("Open(unknown) = nil error")
	}
}

// TestManagerHealsVanishedWorkspace: a workspace deleted OUT OF BAND — an
// operator removed it at the provider, another node reaped it, the machine
// was lost — must not wedge the identity row on a dead reference. The first
// exec after that recreates and runs, and says so; the row is rewritten.
func TestManagerHealsVanishedWorkspace(t *testing.T) {
	s := newTestStore(t)
	f := &fakeProvider{}
	m := NewManager(f, Limits{}, s)
	spec := Spec{Tenant: "acme", Stack: "agents", Name: "tools"}
	ctx := context.Background()

	if _, _, _, err := m.Exec(ctx, spec, ExecRequest{Command: "one"}); err != nil {
		t.Fatal(err)
	}
	// Someone deletes the workspace behind our back: the provider now says
	// it is gone, and a fresh Manager (another node) holds no cache.
	f.vanishOnce = true
	m2 := NewManager(f, Limits{}, s)
	m2.now = m.now
	created := f.created

	res, _, _, err := m2.Exec(ctx, spec, ExecRequest{Command: "two"})
	if err != nil {
		t.Fatalf("a vanished workspace did not heal: %v", err)
	}
	if !res.Recreated {
		t.Error("result does not report that the workspace was recreated")
	}
	if f.created != created+1 {
		t.Errorf("Create called %d times, want one more than %d", f.created, created)
	}
	row, _ := s.Get(ctx, "acme", "agents", "tools")
	if row == nil || row.Status == StatusDestroyed || row.DestroyedAt != nil {
		t.Errorf("row not rewritten after the heal: %+v", row)
	}
}

// TestManagerDoesNotRecreateOnTransientWakeFailure is the other half: a wake
// that failed for any OTHER reason must never be healed by recreating —
// that would silently replace a workspace's files with an empty one.
func TestManagerDoesNotRecreateOnTransientWakeFailure(t *testing.T) {
	s := newTestStore(t)
	f := &fakeProvider{}
	m := NewManager(f, Limits{}, s)
	spec := Spec{Tenant: "acme", Stack: "agents", Name: "tools"}
	ctx := context.Background()

	if _, _, _, err := m.Exec(ctx, spec, ExecRequest{Command: "one"}); err != nil {
		t.Fatal(err)
	}
	created := f.created
	f.wakeErr = errors.New("provider had a bad day")

	if _, _, _, err := m.Exec(ctx, spec, ExecRequest{Command: "two"}); err == nil {
		t.Fatal("a transient wake failure was swallowed")
	}
	if f.created != created {
		t.Errorf("Create called %d times, want %d — a transient failure must not recreate", f.created, created)
	}
	row, _ := s.Get(ctx, "acme", "agents", "tools")
	if row.Status == StatusDestroyed {
		t.Error("a transient wake failure marked the row destroyed")
	}
	// Once the provider recovers, the same workspace is used again.
	f.wakeErr = nil
	if _, h, _, err := m.Exec(ctx, spec, ExecRequest{Command: "three"}); err != nil || h.Ref != "ref/tools" {
		t.Errorf("did not recover onto the SAME workspace: %+v %v", h, err)
	}
	if f.created != created {
		t.Errorf("recovery recreated the workspace (%d creates)", f.created)
	}
}

func TestAppStack(t *testing.T) {
	for in, want := range map[string]string{
		"ws-hello":              "ws-hello",
		"ws-hello/_websocket":   "ws-hello",
		"ws-hello/_mail":        "ws-hello",
		"ws-hello/_websocket/x": "ws-hello",
		"site/canary":           "site/canary", // a nested stack with no _ segment is left alone
		"":                      "",
		"_sys/boot":             "_sys/boot",
	} {
		if got := AppStack(in); got != want {
			t.Errorf("AppStack(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestProviderNameSanitizesEveryComponent: a provider-side identifier has to
// be a DNS-label-shaped token. The workspace NAME is validated, but the
// tenant and stack are not — an inlet sub-stack (`app/_websocket`) or a
// dotted tenant slug would otherwise produce a name the provider rejects
// (seen in production: "create: API error (status 400)").
func TestProviderNameSanitizesEveryComponent(t *testing.T) {
	got := ProviderName("onepony", "ws-hello/_websocket", "tools")
	for _, bad := range []string{"/", "_", ".", " "} {
		if strings.Contains(got, bad) {
			t.Errorf("ProviderName = %q, contains %q", got, bad)
		}
	}
	if !segmentRE.MatchString(got) {
		t.Errorf("ProviderName = %q, which is not a DNS label", got)
	}
	// Identity still separates: the hash covers the raw components.
	if ProviderName("onepony", "ws-hello/_websocket", "tools") == ProviderName("onepony", "ws-hello", "tools") {
		t.Error("two different identities produced the same provider name")
	}
	// A dotted tenant slug is fine too.
	if d := ProviderName("acme.example", "site", "a_b"); !segmentRE.MatchString(d) {
		t.Errorf("ProviderName = %q, which is not a DNS label", d)
	}
}
