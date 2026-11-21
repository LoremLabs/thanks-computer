package attached

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestCodeIsSpentOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	code, expires, err := r.svc.Invite(ctx, testSpec, "Rain", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ttl := time.Until(expires); ttl < 59*time.Minute || ttl > 61*time.Minute {
		t.Fatalf("default life = %s", ttl)
	}
	if info, err := r.store.PeekCode(ctx, code); err != nil || info.Label != "Rain" {
		t.Fatalf("peek = %+v, %v", info, err)
	}
	// Peeking spends nothing.
	if _, err := r.store.PeekCode(ctx, code); err != nil {
		t.Fatalf("a second peek: %v", err)
	}
	pub, _ := newKey(t)
	cred, replaced, err := r.store.Enroll(ctx, code, Enrollment{PublicKey: pub, Label: "the machine's own idea of a label"})
	if err != nil || len(replaced) != 0 {
		t.Fatalf("enroll: %v, replaced %v", err, replaced)
	}
	// One credential names one workspace, and it comes from the CODE.
	if cred.Tenant != testSpec.Tenant || cred.Stack != testSpec.Stack || cred.Name != testSpec.Name ||
		cred.WorkspaceID != workspace.ID(testSpec.Tenant, testSpec.Stack, testSpec.Name) {
		t.Fatalf("credential = %+v", cred)
	}
	if cred.Label != "Rain" {
		t.Fatalf("label = %q: the machine chose it, not the owning stack", cred.Label)
	}
	if cred.Fingerprint == "" || cred.Fingerprint != Fingerprint(pub) {
		t.Fatalf("fingerprint = %q", cred.Fingerprint)
	}
	pub2, _ := newKey(t)
	if _, _, err := r.store.Enroll(ctx, code, Enrollment{PublicKey: pub2}); !errors.Is(err, ErrBadCode) {
		t.Fatalf("the code a second time: %v", err)
	}
	if _, err := r.store.PeekCode(ctx, code); !errors.Is(err, ErrBadCode) {
		t.Fatalf("peek at a spent code: %v", err)
	}
	if _, _, err := r.store.Enroll(ctx, "not-a-code", Enrollment{PublicKey: pub2}); !errors.Is(err, ErrBadCode) {
		t.Fatalf("a made-up code: %v", err)
	}
	// The row is what says the name is attached.
	row, err := r.ws.Get(ctx, testSpec.Tenant, testSpec.Stack, testSpec.Name)
	if err != nil || row == nil || row.Provider != workspace.ProviderAttached || row.ProviderRef != cred.ID {
		t.Fatalf("row = %+v, %v", row, err)
	}
}

func TestCodeRacedByTwoMachines(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	code, _, err := r.svc.Invite(ctx, testSpec, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 8; i++ {
		pub, _ := newKey(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := r.store.Enroll(ctx, code, Enrollment{PublicKey: pub})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case !errors.Is(err, ErrBadCode):
				t.Errorf("enroll: %v", err)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d machines enrolled with one code", won)
	}
}

func TestExpiredCode(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	now := time.Now().UTC()
	r.store.now = func() time.Time { return now }
	code, expires, err := r.svc.Invite(ctx, testSpec, "", 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got := expires.Sub(now); got != MaxCodeTTL {
		t.Fatalf("a code asked to live 72h lives %s", got)
	}
	now = now.Add(MaxCodeTTL + time.Second)
	pub, _ := newKey(t)
	if _, _, err := r.store.Enroll(ctx, code, Enrollment{PublicKey: pub}); !errors.Is(err, ErrBadCode) {
		t.Fatalf("an expired code: %v", err)
	}
}

// Enrolling never destroys anything: a name the provider holds is refused,
// at the invite and again at the enroll, and the code survives the refusal.
func TestProviderHeldNameIsRefused(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	held := workspace.Spec{Tenant: "acme", Stack: "loop", Name: "pony/rain"}
	if err := r.ws.Upsert(ctx, workspace.Row{Tenant: held.Tenant, Stack: held.Stack, Name: held.Name, Provider: "sprites", ProviderRef: "sprite-1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.svc.Invite(ctx, held, "", 0); !errors.Is(err, ErrNameInUse) {
		t.Fatalf("invite for a provider's name: %v", err)
	}

	// The provider takes the name AFTER the code was made.
	code, _, err := r.svc.Invite(ctx, testSpec, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ws.Upsert(ctx, workspace.Row{Tenant: testSpec.Tenant, Stack: testSpec.Stack, Name: testSpec.Name, Provider: "sprites", ProviderRef: "sprite-2"}); err != nil {
		t.Fatal(err)
	}
	pub, _ := newKey(t)
	if _, _, err := r.store.Enroll(ctx, code, Enrollment{PublicKey: pub}); !errors.Is(err, ErrNameInUse) {
		t.Fatalf("enroll over a provider's name: %v", err)
	}
	row, _ := r.ws.Get(ctx, testSpec.Tenant, testSpec.Stack, testSpec.Name)
	if row.Provider != "sprites" || row.ProviderRef != "sprite-2" {
		t.Fatalf("the provider's row was touched: %+v", row)
	}
	// Once that workspace is destroyed, the SAME code still works.
	if err := r.ws.MarkDestroyed(ctx, row.ID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.store.Enroll(ctx, code, Enrollment{PublicKey: pub}); err != nil {
		t.Fatalf("enroll after the provider's workspace was destroyed: %v", err)
	}
}

// One live credential for a name. A new enrollment revokes the old one, and
// the old machine's link is refused from then on.
func TestNewEnrollmentRevokesTheOld(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	old, oldKey := r.enroll(testSpec)
	m := r.mustConnect(old, oldKey, handlerFunc{request: echo})
	mine, _, _ := r.claims.Read(ctx, testSpec.Tenant, old.WorkspaceID)

	second, _, err := r.svc.Invite(ctx, testSpec, "", 0)
	if err != nil {
		t.Fatalf("a second code for an attached name: %v", err)
	}
	pub, _ := newKey(t)
	fresh, replaced, err := r.store.Enroll(ctx, second, Enrollment{PublicKey: pub})
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != 1 || replaced[0] != old.ID {
		t.Fatalf("replaced = %v, want the old credential", replaced)
	}
	if c, _ := r.store.Credential(ctx, old.ID); !c.Revoked() {
		t.Fatal("the old credential is still live")
	}
	if c, _ := r.store.Live(ctx, old.WorkspaceID); c == nil || c.ID != fresh.ID {
		t.Fatalf("live credential = %+v", c)
	}
	// The old machine's link is still open and its claim still installed;
	// the credential check alone keeps requests from it.
	if _, err := r.svc.Hub.Send(ctx, old.WorkspaceID, mine.Conn, mine.Gen, Header{Kind: KindRequest, ID: "req_old"}); code(err) != workspace.CodeAway {
		t.Fatalf("a request to the replaced machine: %v", err)
	}
	if err := <-m.linkErr; !Final(err) {
		t.Fatalf("the old machine's link ended with %v", err)
	}
}

// fakeProvider counts what the Manager asks of it.
type fakeProvider struct {
	mu                      sync.Mutex
	creates, wakes, destroy int
}

type fakeComputer struct{}

func (fakeComputer) Exec(context.Context, workspace.ExecRequest, workspace.Limits) (workspace.ExecResult, error) {
	return workspace.ExecResult{Stdout: []byte("from the provider")}, nil
}

func (p *fakeProvider) Name() string           { return "fake" }
func (p *fakeProvider) Capabilities() []string { return []string{"exec"} }
func (p *fakeProvider) Create(_ context.Context, s workspace.Spec) (workspace.Handle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	return workspace.Handle{Provider: "fake", Ref: "fake-" + s.Name, Name: s.Name}, nil
}
func (p *fakeProvider) Wake(context.Context, workspace.Handle) (workspace.Computer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wakes++
	return fakeComputer{}, nil
}
func (p *fakeProvider) Sleep(context.Context, workspace.Handle) error { return nil }
func (p *fakeProvider) Destroy(context.Context, workspace.Handle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroy++
	return nil
}
func (p *fakeProvider) Status(context.Context, workspace.Handle) (workspace.Status, error) {
	return workspace.Status{State: "running"}, nil
}

func (p *fakeProvider) counts() (int, int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates, p.wakes, p.destroy
}

// The row decides. An attached name is never created, woken or destroyed
// through the provider, whether or not its machine is connected; and a
// destroy gives the name back.
func TestManagerNeverHandsAnAttachedNameToTheProvider(t *testing.T) {
	r := newRig(t)
	prov := &fakeProvider{}
	mgr := workspace.NewManager(prov, workspace.Limits{}, r.ws)
	mgr.SetAttached(r.svc)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cred, key := r.enroll(testSpec)

	// Enrolled, nobody connected: away, at once, and the provider is not asked.
	if _, _, _, err := mgr.Exec(ctx, testSpec, workspace.ExecRequest{Command: "true"}); code(err) != workspace.CodeAway {
		t.Fatalf("exec with no machine: %v, want away", err)
	}
	if _, _, err := mgr.Wake(ctx, testSpec); code(err) != workspace.CodeAway {
		t.Fatalf("wake with no machine: %v, want away", err)
	}

	m := r.mustConnect(cred, key, handlerFunc{request: echo})
	res, h, _, err := mgr.Exec(ctx, testSpec, workspace.ExecRequest{Command: "ls"})
	if err != nil || string(res.Stdout) != "ran: ls" {
		t.Fatalf("exec = %+v, %v", res, err)
	}
	if h.Provider != workspace.ProviderAttached || h.Ref != cred.ID {
		t.Fatalf("handle = %+v", h)
	}
	if err := mgr.Sleep(ctx, testSpec); code(err) != "unsupported" {
		t.Fatalf("sleep: %v", err)
	}
	if st, err := mgr.Status(ctx, testSpec); err != nil || st.State != workspace.ProviderAttached {
		t.Fatalf("status = %+v, %v", st, err)
	}

	// The machine drops mid-life: still away, still not the provider's.
	m.cancel()
	<-m.hubErr
	if _, _, _, err := mgr.Exec(ctx, testSpec, workspace.ExecRequest{Command: "true"}); code(err) != workspace.CodeAway {
		t.Fatalf("exec after the machine left: %v, want away", err)
	}
	if c, w, d := prov.counts(); c+w+d != 0 {
		t.Fatalf("the provider was asked: %d creates, %d wakes, %d destroys", c, w, d)
	}

	// Another name is the provider's as ever.
	other := workspace.Spec{Tenant: "acme", Stack: "loop", Name: "pony/rain"}
	if res, _, _, err := mgr.Exec(ctx, other, workspace.ExecRequest{Command: "true"}); err != nil || string(res.Stdout) != "from the provider" {
		t.Fatalf("a provider name = %+v, %v", res, err)
	}

	// Destroy: nothing is destroyed at the provider, the credential is
	// revoked, and the name is the provider's again.
	if err := mgr.Destroy(ctx, testSpec); err != nil {
		t.Fatal(err)
	}
	if _, _, d := prov.counts(); d != 0 {
		t.Fatal("destroying an attached name destroyed something at the provider")
	}
	if c, _ := r.store.Credential(ctx, cred.ID); !c.Revoked() {
		t.Fatal("the credential outlived its workspace")
	}
	if _, ok := r.connect(cred.ID, key, testHost, handlerFunc{request: echo}); ok {
		t.Fatal("the machine reconnected after its workspace was destroyed")
	}
	if res, _, _, err := mgr.Exec(ctx, testSpec, workspace.ExecRequest{Command: "true"}); err != nil || string(res.Stdout) != "from the provider" {
		t.Fatalf("the name after destroy = %+v, %v", res, err)
	}
}

// A node whose cache still holds a provider handle for a name that has since
// been enrolled must not mark the attached row destroyed when that handle
// turns out dead.
func TestStaleProviderHandleDoesNotDestroyAnAttachedRow(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	wid := workspace.ID(testSpec.Tenant, testSpec.Stack, testSpec.Name)
	r.enroll(testSpec)
	// What dropIdentity does for a provider "fake" that found its handle gone.
	if err := r.ws.MarkDestroyedFor(ctx, wid, "fake", time.Time{}); err != nil {
		t.Fatal(err)
	}
	row, _ := r.ws.Get(ctx, testSpec.Tenant, testSpec.Stack, testSpec.Name)
	if row.Status == workspace.StatusDestroyed {
		t.Fatal("an attached row was marked destroyed by another provider's heal")
	}
	// And a provider recording a workspace it made for the name is refused.
	applied, err := r.ws.UpsertUnlessAttached(ctx, workspace.Row{Tenant: testSpec.Tenant, Stack: testSpec.Stack, Name: testSpec.Name, Provider: "fake", ProviderRef: "x"})
	if err != nil || applied {
		t.Fatalf("a provider wrote over an attached row: applied %v, %v", applied, err)
	}
}
