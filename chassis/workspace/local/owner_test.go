package local

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/attached/home"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
	"github.com/loremlabs/thanks-computer/chassis/workspace/detach"
)

const ownerCred = "atc-0123456789abcdef0123456789abcdef"

// newOwner makes an attach folder holding one credential rooted at root.
func newOwner(t *testing.T) (*Owner, home.Home, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := home.Home{Dir: filepath.Join(base, "attach")}
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	if err := h.Save(home.Credential{ID: ownerCred, Address: "https://attach.example.test", Root: root, Seed: key.Seed(), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	o, err := NewOwner(h, []string{os.Args[0], detach.HiddenCommand})
	if err != nil {
		t.Fatal(err)
	}
	return o, h, root
}

func ownerComp(t *testing.T, o *Owner, name string) workspace.Computer {
	t.Helper()
	hnd, err := o.Create(context.Background(), workspace.Spec{Tenant: OwnerTenant, Stack: "exec", Name: name})
	if err != nil {
		t.Fatal(err)
	}
	c, err := o.Wake(context.Background(), hnd)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func wsErr(err error) string {
	var we *workspace.Error
	if errors.As(err, &we) {
		return we.Code
	}
	return ""
}

func TestOwnerRunsInTheRootAsTheOwner(t *testing.T) {
	o, h, root := newOwner(t)
	SetOwnerEnv([]string{"PATH=/usr/bin:/bin", "HOME=/home/the-owner", "OWNERS_OWN=kept", "TXCO_ATTACH_ROOT=spoofed"})
	t.Cleanup(func() { SetOwnerEnv(nil) })
	c := ownerComp(t, o, ownerCred)

	res, err := c.Exec(context.Background(), workspace.ExecRequest{
		Command: `pwd; echo "$HOME|$OWNERS_OWN|$FROM_REQUEST|$TXCO_ATTACH_ROOT|$TXCO_ATTACH_STATE"`,
		Env:     map[string]string{"FROM_REQUEST": "yes", "TXCO_ATTACH_STATE": "spoofed too"},
	}, workspace.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	// The owner's environment, then the request's, then the two names the
	// provider sets last: neither the owner's shell nor the request can
	// point them elsewhere.
	want := root + "\n/home/the-owner|kept|yes|" + root + "|" + h.StateDir(ownerCred) + "\n"
	if string(res.Stdout) != want {
		t.Fatalf("stdout = %q\nwant     %q", res.Stdout, want)
	}
	if st, err := os.Stat(h.StateDir(ownerCred)); err != nil || !st.IsDir() {
		t.Fatalf("the state folder was not made: %v", err)
	}
}

// The root is where work starts, not a fence.
func TestOwnerCwd(t *testing.T) {
	o, _, root := newOwner(t)
	c := ownerComp(t, o, ownerCred)
	usr, _ := filepath.EvalSymlinks("/usr")
	for cwd, want := range map[string]string{"": root, "sub": filepath.Join(root, "sub"), "/usr": usr} {
		res, err := c.Exec(context.Background(), workspace.ExecRequest{Command: "pwd -P", Cwd: cwd}, workspace.Limits{})
		if err != nil {
			t.Fatalf("cwd %q: %v", cwd, err)
		}
		if got := strings.TrimSpace(string(res.Stdout)); got != want {
			t.Errorf("cwd %q ran in %s, want %s", cwd, got, want)
		}
	}
	// A directory that is not there is refused; the command is never
	// started somewhere else instead.
	for _, cwd := range []string{"nowhere", "/no/such/dir"} {
		if _, err := c.Exec(context.Background(), workspace.ExecRequest{Command: "pwd", Cwd: cwd}, workspace.Limits{}); wsErr(err) != "bad_request" {
			t.Errorf("cwd %q: %v, want bad_request", cwd, err)
		}
	}
}

func TestOwnerServesOnlyItsCredentialsAndItsTenant(t *testing.T) {
	o, h, root := newOwner(t)
	for _, name := range []string{"tools", "atc-ffffffffffffffffffffffffffffffff", "computer/rain"} {
		if _, err := o.Create(context.Background(), workspace.Spec{Tenant: OwnerTenant, Stack: "exec", Name: name}); wsErr(err) != workspace.CodeNotFound {
			t.Errorf("name %q: %v, want not_found", name, err)
		}
	}
	if o.ServesTenant("acme") || o.ServesTenant("_sys") || !o.ServesTenant(OwnerTenant) {
		t.Fatal("the owner provider serves the wrong tenants")
	}
	// A credential its chassis ended is not served.
	hnd, _ := o.Create(context.Background(), workspace.Spec{Tenant: OwnerTenant, Stack: "exec", Name: ownerCred})
	if err := h.End(ownerCred, "revoked"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Wake(context.Background(), hnd); wsErr(err) != workspace.CodeNotFound {
		t.Fatalf("an ended credential: %v", err)
	}
	// Destroy removes nothing: the folder is its owner's.
	if err := o.Destroy(context.Background(), hnd); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("destroy removed the owner's folder")
	}
}

// The root lives in the credential's file and nowhere else, so a folder that
// has gone is seen at the next command, and nothing runs elsewhere instead.
func TestOwnerRootGone(t *testing.T) {
	o, _, root := newOwner(t)
	hnd, _ := o.Create(context.Background(), workspace.Spec{Tenant: OwnerTenant, Stack: "exec", Name: ownerCred})
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Wake(context.Background(), hnd); wsErr(err) != "unavailable" {
		t.Fatalf("a root that is gone: %v", err)
	}
}

func TestOwnerDetached(t *testing.T) {
	o, _, root := newOwner(t)
	SetOwnerEnv([]string{"PATH=/usr/bin:/bin", "OWNERS_OWN=kept"})
	t.Cleanup(func() { SetOwnerEnv(nil) })
	c := ownerComp(t, o, ownerCred)
	job, err := c.(workspace.Detacher).ExecDetached(context.Background(), workspace.ExecRequest{
		Command: `echo "$(pwd -P) $OWNERS_OWN $TXCO_ATTACH_ROOT"; exit 6`, Cwd: "sub", Timeout: time.Minute,
	}, workspace.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for stop := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		state, _, err := o.Jobs().Status(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state == detach.Ended {
			break
		}
		if state == detach.Lost || time.Now().After(stop) {
			t.Fatalf("job is %s", state)
		}
	}
	res, _, err := o.Jobs().Result(job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "sub") + " kept " + root + "\n"
	if string(res.Stdout) != want || res.Exit != 6 {
		t.Fatalf("exit %d stdout %q, want 6 and %q", res.Exit, res.Stdout, want)
	}
}
