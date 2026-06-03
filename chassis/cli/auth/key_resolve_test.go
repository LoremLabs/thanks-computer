package auth

import (
	"bytes"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
)

// withAgent replaces agentDialer with one returning an in-memory
// keyring loaded with `keys`. Restores on test cleanup.
func withAgent(t *testing.T, keys ...ed25519.PrivateKey) {
	t.Helper()
	a := agent.NewKeyring()
	for i, k := range keys {
		if err := a.Add(agent.AddedKey{
			PrivateKey: k,
			Comment:    "test-" + string(rune('a'+i)),
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	prev := agentDialer
	agentDialer = func() (agent.Agent, error) { return a, nil }
	t.Cleanup(func() { agentDialer = prev })
}

// withoutAgent installs an agentDialer that always reports unreachable.
// Models the common case "this machine has no ssh-agent running."
func withoutAgent(t *testing.T) {
	t.Helper()
	prev := agentDialer
	agentDialer = func() (agent.Agent, error) { return nil, signer.ErrNoAgent }
	t.Cleanup(func() { agentDialer = prev })
}

// withFakeHome points userHomeDir at a temp directory so the
// ~/.ssh/id_ed25519 auto-fallback can be exercised without touching
// the developer's real $HOME.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	prev := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = prev })
	return home
}

// writeOpenSSHKey writes priv to dir/name in OpenSSH PEM format.
// Used to seed `~/.ssh/id_ed25519` or `--ssh-key` paths.
func writeOpenSSHKey(t *testing.T, dir, name string, priv ed25519.PrivateKey) string {
	t.Helper()
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClassifyExistingEnrolmentNone — empty $TXCO_HOME, no meta on
// disk: first-time setup. The probe must not run.
func TestClassifyExistingEnrolmentNone(t *testing.T) {
	withHome(t)
	probeCalled := false
	probe := func(client.Target) (bool, error) {
		probeCalled = true
		return false, nil
	}
	kind, meta, err := classifyExistingEnrolment("local", "http://localhost:8081", probe)
	if kind != EnrolmentNone {
		t.Errorf("kind = %v, want EnrolmentNone", kind)
	}
	if meta != nil {
		t.Errorf("meta = %v, want nil", meta)
	}
	if err != nil {
		t.Errorf("probeErr = %v, want nil", err)
	}
	if probeCalled {
		t.Errorf("probe must not run when there's no meta")
	}
}

// TestClassifyExistingEnrolmentOtherChassis — meta exists for a
// different chassis URL. The probe must not run (the answer is
// "wrong chassis" regardless of whether the key would work).
func TestClassifyExistingEnrolmentOtherChassis(t *testing.T) {
	withHome(t)
	mp, _ := MetaPath("local")
	_ = os.MkdirAll(filepath.Dir(mp), 0o700)
	if err := SaveMeta(mp, Meta{
		ActorID:    "actor_test",
		ChassisURL: "http://other.example.com:8081",
	}); err != nil {
		t.Fatal(err)
	}
	probeCalled := false
	probe := func(client.Target) (bool, error) {
		probeCalled = true
		return true, nil
	}
	kind, meta, _ := classifyExistingEnrolment("local", "http://localhost:8081", probe)
	if kind != EnrolmentOtherChassis {
		t.Errorf("kind = %v, want EnrolmentOtherChassis", kind)
	}
	if meta == nil || meta.ActorID != "actor_test" {
		t.Errorf("meta = %v, want actor_test", meta)
	}
	if probeCalled {
		t.Errorf("probe must not run when chassis URL doesn't match")
	}
}

// TestClassifyExistingEnrolmentRejected — same chassis URL but the
// probe says the key is rejected (the chassis was rebuilt, or the
// key revoked). Caller falls through to the recovery prompt.
//
// Hits the "signer missing" sub-path: meta exists but no key file
// is on disk, so buildSignedTarget returns Auth=nil and the
// classifier returns Rejected without ever invoking the probe.
func TestClassifyExistingEnrolmentRejected(t *testing.T) {
	withHome(t)
	mp, _ := MetaPath("local")
	_ = os.MkdirAll(filepath.Dir(mp), 0o700)
	if err := SaveMeta(mp, Meta{
		ActorID:    "actor_test",
		ChassisURL: "http://localhost:8081",
		KeySource:  "file",
		KeyPath:    "/nonexistent/key.ed25519", // forces signer load to fail
	}); err != nil {
		t.Fatal(err)
	}
	probe := func(client.Target) (bool, error) {
		t.Fatal("probe must not run when the signer can't be loaded")
		return false, nil
	}
	kind, _, _ := classifyExistingEnrolment("local", "http://localhost:8081", probe)
	if kind != EnrolmentRejected {
		t.Errorf("kind = %v, want EnrolmentRejected (signer load failed)", kind)
	}
}

// TestResolveConflictingFlagsErrors — passing more than one of the
// mutually-exclusive flags must error early.
func TestResolveConflictingFlagsErrors(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	cases := []EnrollmentChoices{
		{SSHAgent: true, SSHKey: "/tmp/k"},
		{SSHAgent: true, NewKey: true},
		{SSHKey: "/tmp/k", NewKey: true},
	}
	for i, c := range cases {
		if _, err := resolveEnrollmentKey(c, strings.NewReader(""), false, new(bytes.Buffer)); err == nil {
			t.Errorf("case %d: expected error on conflicting flags %+v", i, c)
		}
	}
}

// TestResolveSSHAgentExplicit — --ssh-agent + one key in agent →
// agent-backed choice with the right fingerprint. No prompting even
// on TTY (the flag is the confirmation).
func TestResolveSSHAgentExplicit(t *testing.T) {
	withHome(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	withAgent(t, priv)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{SSHAgent: true},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.KeySource != SourceSSHAgent {
		t.Errorf("KeySource=%q, want %q", ek.KeySource, SourceSSHAgent)
	}
	pub, _ := priv.Public().(ed25519.PublicKey)
	if !pub.Equal(ek.PublicKey) {
		t.Errorf("PublicKey mismatch")
	}
	if !strings.HasPrefix(ek.Fingerprint, "SHA256:") {
		t.Errorf("Fingerprint=%q, want SHA256: prefix", ek.Fingerprint)
	}
}

// TestResolveSSHAgentNoIdentities — --ssh-agent against an agent
// with no Ed25519 keys is a hard error (the user asked for the
// agent path specifically; we can't silently fall through).
func TestResolveSSHAgentNoIdentities(t *testing.T) {
	withHome(t)
	withAgent(t) // empty keyring
	_, err := resolveEnrollmentKey(EnrollmentChoices{SSHAgent: true},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err == nil {
		t.Fatal("expected error when --ssh-agent meets an empty keyring")
	}
	if !errors.Is(err, signer.ErrNoAgent) {
		t.Errorf("got %v, want ErrNoAgent wrap", err)
	}
}

// TestResolveSSHAgentMultiNonTTY — multiple agent keys without a
// TTY must error and list the candidates so the user can pick.
func TestResolveSSHAgentMultiNonTTY(t *testing.T) {
	withHome(t)
	_, p1, _ := ed25519.GenerateKey(nil)
	_, p2, _ := ed25519.GenerateKey(nil)
	withAgent(t, p1, p2)

	var stderr bytes.Buffer
	_, err := resolveEnrollmentKey(EnrollmentChoices{SSHAgent: true},
		strings.NewReader(""), false, &stderr)
	if err == nil {
		t.Fatal("expected error on multi-key + non-TTY")
	}
	if !strings.Contains(stderr.String(), "[1]") || !strings.Contains(stderr.String(), "[2]") {
		t.Errorf("stderr should list candidates; got %q", stderr.String())
	}
}

// TestResolveSSHAgentMultiTTYPicksFirst — user types "1" at the
// prompt; the resolver picks the first listed agent key.
func TestResolveSSHAgentMultiTTYPicksFirst(t *testing.T) {
	withHome(t)
	_, p1, _ := ed25519.GenerateKey(nil)
	_, p2, _ := ed25519.GenerateKey(nil)
	withAgent(t, p1, p2)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{SSHAgent: true},
		strings.NewReader("1\n"), true, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pub1, _ := p1.Public().(ed25519.PublicKey)
	if !pub1.Equal(ek.PublicKey) {
		t.Errorf("expected first key, got different pubkey")
	}
}

// TestReadPubCommentBesides — when ssh-keygen writes the .pub
// sidecar with a comment ("ssh-ed25519 AAAA… matt@laptop"), the
// helper recovers that third field. This is how the CLI defaults
// --label from a file-backed key's identity.
func TestReadPubCommentBesides(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(nil)
	priv2Path := writeOpenSSHKey(t, dir, "id_x", priv)

	// Hand-write a sidecar like ssh-keygen would.
	sshPub, _ := ssh.NewPublicKey(priv.Public().(ed25519.PublicKey))
	pubLine := ssh.MarshalAuthorizedKey(sshPub) // includes trailing \n
	// Append a comment as ssh-keygen does.
	withComment := strings.TrimRight(string(pubLine), "\n") + " matt@laptop\n"
	if err := os.WriteFile(priv2Path+".pub", []byte(withComment), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := readPubCommentBesides(priv2Path); got != "matt@laptop" {
		t.Errorf("readPubCommentBesides = %q, want matt@laptop", got)
	}
}

// TestReadPubCommentBesidesNoSidecar — no .pub file alongside:
// returns "" without erroring.
func TestReadPubCommentBesidesNoSidecar(t *testing.T) {
	dir := t.TempDir()
	_, priv, _ := ed25519.GenerateKey(nil)
	p := writeOpenSSHKey(t, dir, "lonely", priv)
	if got := readPubCommentBesides(p); got != "" {
		t.Errorf("expected empty comment when no .pub sidecar; got %q", got)
	}
}

// TestResolveSSHKeyExplicit — --ssh-key <path> reads that file's
// public half. No prompting; the path itself is the confirmation.
func TestResolveSSHKeyExplicit(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(nil)
	path := writeOpenSSHKey(t, dir, "myssh", priv)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{SSHKey: path},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.KeySource != SourceFile {
		t.Errorf("KeySource=%q, want %q", ek.KeySource, SourceFile)
	}
	if ek.KeyPath != path {
		t.Errorf("KeyPath=%q, want %q", ek.KeyPath, path)
	}
	if !pub.Equal(ek.PublicKey) {
		t.Errorf("PublicKey mismatch")
	}
}

// TestResolveNewKey — --new-key with no agent + no flag generates a
// fresh key in memory, ready to PersistFreshKey() after enrolment.
func TestResolveNewKey(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	ek, err := resolveEnrollmentKey(EnrollmentChoices{NewKey: true, Name: "local"},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.privKey == nil {
		t.Errorf("--new-key should set privKey on EnrollmentKey")
	}
	if ek.persistPath == "" {
		t.Errorf("--new-key should set persistPath")
	}
	// Persisting writes to disk.
	if err := ek.PersistFreshKey("matt@laptop"); err != nil {
		t.Fatalf("PersistFreshKey: %v", err)
	}
	if _, err := os.Stat(ek.persistPath); err != nil {
		t.Errorf("expected key at %q after persist: %v", ek.persistPath, err)
	}
	// .pub sidecar should also be written so the file is
	// drop-in-compatible with standard SSH tooling.
	pubData, err := os.ReadFile(ek.persistPath + ".pub")
	if err != nil {
		t.Fatalf("expected .pub sidecar: %v", err)
	}
	if !bytes.HasPrefix(pubData, []byte("ssh-ed25519 ")) {
		t.Errorf(".pub should start with `ssh-ed25519 `; got %q", pubData)
	}
	if !bytes.Contains(pubData, []byte("matt@laptop")) {
		t.Errorf(".pub should include the --label comment; got %q", pubData)
	}
}

// TestResolveDefaultGeneratesAtSSHTxcoPathWhenMissing — no flags, no
// pre-existing ~/.ssh/id_ed25519-txco: the resolver returns an
// EnrollmentKey staged to write at exactly that path. The file is NOT
// yet on disk; PersistFreshKey commits it after enrolment confirms.
func TestResolveDefaultGeneratesAtSSHTxcoPathWhenMissing(t *testing.T) {
	withHome(t)
	home := withFakeHome(t)
	want := filepath.Join(home, ".ssh", "id_ed25519-txco")

	ek, err := resolveEnrollmentKey(EnrollmentChoices{},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.KeyPath != want {
		t.Errorf("KeyPath=%q, want %q", ek.KeyPath, want)
	}
	if ek.persistPath != want {
		t.Errorf("persistPath=%q, want %q", ek.persistPath, want)
	}
	if ek.privKey == nil {
		t.Errorf("fresh-keygen path should populate privKey")
	}
	if _, err := os.Stat(want); err == nil {
		t.Errorf("default path should not be written until PersistFreshKey; found %q on disk", want)
	}
	// Persist + verify both files appear.
	if err := ek.PersistFreshKey("matt@laptop"); err != nil {
		t.Fatalf("PersistFreshKey: %v", err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("private key at %q after persist: %v", want, err)
	}
	pubData, err := os.ReadFile(want + ".pub")
	if err != nil {
		t.Fatalf(".pub sidecar at %q: %v", want+".pub", err)
	}
	if !bytes.HasPrefix(pubData, []byte("ssh-ed25519 ")) {
		t.Errorf(".pub must start with `ssh-ed25519 `; got %q", pubData)
	}
}

// TestResolveDefaultReusesExistingSSHTxcoFile — when
// ~/.ssh/id_ed25519-txco already exists, the resolver loads its pubkey
// (matches --ssh-key semantics) instead of writing a new file. No
// privKey/persistPath set: nothing to write later.
func TestResolveDefaultReusesExistingSSHTxcoFile(t *testing.T) {
	withHome(t)
	home := withFakeHome(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	written := writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519-txco", priv)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.KeySource != SourceFile {
		t.Errorf("KeySource=%q, want %q (existing file → load)", ek.KeySource, SourceFile)
	}
	if ek.KeyPath != written {
		t.Errorf("KeyPath=%q, want %q", ek.KeyPath, written)
	}
	if !pub.Equal(ek.PublicKey) {
		t.Errorf("PublicKey mismatch — resolver must load the pubkey of the existing file")
	}
	if ek.privKey != nil || ek.persistPath != "" {
		t.Errorf("reuse path must NOT stage a fresh write; privKey=%v persistPath=%q",
			ek.privKey != nil, ek.persistPath)
	}
}

// TestResolveDefaultCreatesSSHDirWith0700 — starting from a home with
// no ~/.ssh/ at all, the resolver creates the dir at mode 0700 (per
// the SSH convention). Pre-existing dirs are not chmod'd.
func TestResolveDefaultCreatesSSHDirWith0700(t *testing.T) {
	withHome(t)
	home := withFakeHome(t)
	sshDir := filepath.Join(home, ".ssh")
	if _, err := os.Stat(sshDir); err == nil {
		t.Fatalf("test setup: %q should not exist yet", sshDir)
	}
	if _, err := resolveEnrollmentKey(EnrollmentChoices{},
		strings.NewReader(""), false, new(bytes.Buffer)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := os.Stat(sshDir)
	if err != nil {
		t.Fatalf("expected %q to exist after resolve: %v", sshDir, err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("~/.ssh mode = %o, want 0700", perm)
	}
}

// TestResolveExplicitNewKeyStillUsesTxcoHome — --new-key keeps its old
// semantics: generate fresh under $TXCO_HOME/keys/<name>.ed25519, NOT
// under ~/.ssh/. The escape hatch for users who don't want a key in
// ~/.ssh/.
func TestResolveExplicitNewKeyStillUsesTxcoHome(t *testing.T) {
	home := withHome(t)
	withoutAgent(t)
	withFakeHome(t) // ensure ~/.ssh stays untouched

	ek, err := resolveEnrollmentKey(EnrollmentChoices{NewKey: true, Name: "local"},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	wantUnder := filepath.Join(home, "keys")
	if !strings.HasPrefix(ek.KeyPath, wantUnder) {
		t.Errorf("KeyPath=%q, want under %q (--new-key must land in $TXCO_HOME, not ~/.ssh/)",
			ek.KeyPath, wantUnder)
	}
	if strings.Contains(ek.KeyPath, ".ssh") {
		t.Errorf("--new-key must not write into ~/.ssh/; got %q", ek.KeyPath)
	}
}

// TestResolveExplicitSSHKeyStillReadsThatPath — --ssh-key <path>
// continues to honor the caller's chosen path; the new default
// doesn't override explicit flags.
func TestResolveExplicitSSHKeyStillReadsThatPath(t *testing.T) {
	withHome(t)
	home := withFakeHome(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	chosen := writeOpenSSHKey(t, filepath.Join(home, "custom"), "my-key", priv)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{SSHKey: chosen},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.KeyPath != chosen {
		t.Errorf("KeyPath=%q, want %q", ek.KeyPath, chosen)
	}
	if !pub.Equal(ek.PublicKey) {
		t.Errorf("PublicKey mismatch — --ssh-key must load the chosen file's pubkey")
	}
}
