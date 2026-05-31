package cli

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/lockfile"
	"github.com/loremlabs/thanks-computer/chassis/cli/sign"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

// sharedStore points both OCI factories at one fresh in-process store, so a
// publish and a subsequent install/pull/inspect round-trip with no network.
func sharedStore(t *testing.T) {
	t.Helper()
	store, err := oci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevPush := source.SetPushRepositoryFactory(func(string) (oras.Target, error) { return store, nil })
	prevPull := source.SetRepositoryFactory(func(string) (oras.ReadOnlyTarget, error) { return store, nil })
	t.Cleanup(func() { source.SetPushRepositoryFactory(prevPush); source.SetRepositoryFactory(prevPull) })
}

// genKey writes an ed25519 keypair to a temp dir, returning the private path,
// the .pub sidecar path, and the key id.
func genKey(t *testing.T) (privPath, pubPath, keyID string) {
	t.Helper()
	privPath = filepath.Join(t.TempDir(), "signing.ed25519")
	pub, priv, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SavePrivateKey(privPath, priv); err != nil {
		t.Fatal(err)
	}
	return privPath, privPath + ".pub", sign.KeyIDForPub(pub)
}

const signRef = "oci://registry.thanks.computer/txco/support-basic:0.1.0"

func TestPublishSignInstallVerifyRoundTrip(t *testing.T) {
	fixture := absFixture(t)
	sharedStore(t)
	privPath, pubPath, keyID := genKey(t)

	var out, errb bytes.Buffer
	if code := runPackage([]string{"publish", "--to", signRef, "--sign", "--key", privPath, fixture}, &out, &errb); code != 0 {
		t.Fatalf("publish --sign: %s", errb.String())
	}
	if !strings.Contains(out.String(), "signed by "+keyID) {
		t.Fatalf("expected 'signed by %s', got: %s", keyID, out.String())
	}

	ws := t.TempDir()
	t.Chdir(ws)
	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"support-basic@0.1.0", "--as", "support", "--require-signature", "--key", pubPath}, &out, &errb); code != 0 {
		t.Fatalf("install --require-signature: out=%s err=%s", out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "verified: signed by "+keyID) {
		t.Errorf("expected verified line, got: %s", out.String())
	}
	lf, _ := lockfile.Read(ws)
	if len(lf.Packages) != 1 || lf.Packages[0].SignedBy != keyID {
		t.Errorf("lockfile SignedBy = %q, want %q", lf.Packages[0].SignedBy, keyID)
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support", "0100_TRIAGE", "classify.txcl")); err != nil {
		t.Errorf("not materialized: %v", err)
	}
}

func TestRequireSignatureUnsignedFailsClosed(t *testing.T) {
	fixture := absFixture(t)
	sharedStore(t)
	_, pubPath, _ := genKey(t)

	var out, errb bytes.Buffer
	if code := runPackage([]string{"publish", "--to", signRef, fixture}, &out, &errb); code != 0 { // no --sign
		t.Fatalf("publish: %s", errb.String())
	}
	ws := t.TempDir()
	t.Chdir(ws)
	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"support-basic@0.1.0", "--as", "support", "--require-signature", "--key", pubPath}, &out, &errb); code == 0 {
		t.Errorf("unsigned + --require-signature should fail; out=%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support")); !os.IsNotExist(err) {
		t.Error("fail-closed must not materialize OPS/")
	}
}

func TestRequireSignatureUntrustedFailsClosed(t *testing.T) {
	fixture := absFixture(t)
	sharedStore(t)
	privPath, _, _ := genKey(t)
	_, otherPub, _ := genKey(t) // a DIFFERENT key the consumer trusts

	var out, errb bytes.Buffer
	if code := runPackage([]string{"publish", "--to", signRef, "--sign", "--key", privPath, fixture}, &out, &errb); code != 0 {
		t.Fatalf("publish --sign: %s", errb.String())
	}
	ws := t.TempDir()
	t.Chdir(ws)
	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"support-basic@0.1.0", "--as", "support", "--require-signature", "--key", otherPub}, &out, &errb); code == 0 {
		t.Errorf("signed-but-untrusted + --require-signature should fail; out=%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support")); !os.IsNotExist(err) {
		t.Error("fail-closed must not materialize OPS/")
	}
}

func TestUnsignedWarnsButInstalls(t *testing.T) {
	fixture := absFixture(t)
	sharedStore(t)

	var out, errb bytes.Buffer
	if code := runPackage([]string{"publish", "--to", signRef, fixture}, &out, &errb); code != 0 {
		t.Fatalf("publish: %s", errb.String())
	}
	ws := t.TempDir()
	t.Chdir(ws)
	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"support-basic@0.1.0", "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("unsigned without --require-signature should succeed: %s", errb.String())
	}
	if !strings.Contains(errb.String(), "warning") {
		t.Errorf("expected a warning for an unsigned install, got: %s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support")); err != nil {
		t.Error("should still materialize without --require-signature")
	}
}

func TestInspectProvenance(t *testing.T) {
	fixture := absFixture(t)
	sharedStore(t)
	privPath, pubPath, keyID := genKey(t)

	var out, errb bytes.Buffer
	if code := runPackage([]string{"publish", "--to", signRef, "--sign", "--key", privPath, fixture}, &out, &errb); code != 0 {
		t.Fatalf("publish: %s", errb.String())
	}
	ws := t.TempDir()
	t.Chdir(ws)
	out.Reset()
	errb.Reset()
	if code := runPackage([]string{"inspect", "support-basic@0.1.0", "--provenance", "--key", pubPath}, &out, &errb); code != 0 {
		t.Fatalf("inspect --provenance: %s", errb.String())
	}
	if !strings.Contains(out.String(), "verified: signed by "+keyID) {
		t.Errorf("inspect should show a verified signature, got: %s", out.String())
	}
}

func TestPackageKeyGenerate(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := runPackage([]string{"key", "generate", "--name", "signing", "--out", dir}, &out, &errb); code != 0 {
		t.Fatalf("key generate: %s", errb.String())
	}
	priv, err := auth.LoadPrivateKey(filepath.Join(dir, "signing.ed25519"))
	if err != nil {
		t.Fatalf("generated key not loadable: %v", err)
	}
	keyID := sign.KeyIDForPub(priv.Public().(ed25519.PublicKey))
	if !strings.Contains(out.String(), keyID) {
		t.Errorf("output should print key id %s, got: %s", keyID, out.String())
	}
	if !strings.Contains(out.String(), "trust:") {
		t.Errorf("output should include a trust: snippet, got: %s", out.String())
	}
}
