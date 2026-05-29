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
		{SSHAgent: true, NoSSHAgent: true},
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

// TestResolveAutoUsesAgentWhenAvailable — no explicit flag + agent
// reachable + one Ed25519 key → use the agent. The "happy default"
// for developers with ssh-agent already running.
func TestResolveAutoUsesAgentWhenAvailable(t *testing.T) {
	withHome(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	withAgent(t, priv)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.KeySource != SourceSSHAgent {
		t.Errorf("KeySource=%q, want %q (auto path should prefer agent)", ek.KeySource, SourceSSHAgent)
	}
}

// TestResolveAutoOffersDefaultSSHKeyPicker — no agent, but
// ~/.ssh/id_ed25519 exists and we're on a TTY: show a numbered
// picker. User selects [1] then confirms with "y" (default N, so
// must type "y" explicitly).
func TestResolveAutoOffersDefaultSSHKeyPicker(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	path := writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", priv)

	var stderr bytes.Buffer
	// First line: pick "1" (the id_ed25519 entry).
	// Second line: confirm "y" (default-N confirmation).
	ek, err := resolveEnrollmentKey(EnrollmentChoices{},
		strings.NewReader("1\ny\n"), true, &stderr)
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
	stderrStr := stderr.String()
	if !strings.Contains(stderrStr, "[1]") || !strings.Contains(stderrStr, "id_ed25519") {
		t.Errorf("picker should list id_ed25519 as [1]; got %q", stderrStr)
	}
	if !strings.Contains(stderrStr, "[y/N]") {
		t.Errorf("confirmation should default to N (capital); got %q", stderrStr)
	}
}

// TestResolveAutoSkipsWithEmptyInput — pressing Enter at the
// picker prompt falls through to fresh keygen (the "skip" option
// is the default). Critical safety property: a developer who isn't
// paying attention doesn't accidentally bind their SSH identity.
func TestResolveAutoSkipsWithEmptyInput(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", priv)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{Name: "local"},
		strings.NewReader("\n"), true, new(bytes.Buffer)) // press enter
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.privKey == nil {
		t.Errorf("empty input should fall through to keygen, not bind ssh-key")
	}
}

// TestResolveAutoConfirmDefaultNDeclines — user picks [1] but then
// hits Enter on the y/N confirmation. Default N means we don't
// enrol — fall through to keygen.
func TestResolveAutoConfirmDefaultNDeclines(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", priv)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{Name: "local"},
		strings.NewReader("1\n\n"), true, new(bytes.Buffer)) // pick 1, then enter (= N)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.privKey == nil {
		t.Errorf("default-N confirm should decline; expected fall-through to keygen")
	}
}

// TestResolveAutoManualPathRetries — user picks the manual-entry
// branch and types a path that doesn't exist (typo). The picker
// should loop and ask again, NOT silently fall through to fresh
// keygen. Confirms the bug-fix for the "typo'd path swallowed"
// regression.
func TestResolveAutoManualPathRetries(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	pub, priv, _ := ed25519.GenerateKey(nil)
	good := writeOpenSSHKey(t, filepath.Join(home, "elsewhere"), "alt", priv)
	// Decoy in ~/.ssh so the picker has a numbered list.
	_, decoyPriv, _ := ed25519.GenerateKey(nil)
	writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", decoyPriv)

	// Inputs:
	//   "2"                        → pick "enter another path"
	//   "/no/such/path"            → bad path, should re-prompt
	//   good                       → correct path, accepted
	//   "y"                        → confirm enrolment
	in := strings.NewReader(
		"2\n" +
			"/no/such/path\n" +
			good + "\n" +
			"y\n",
	)
	var stderr bytes.Buffer
	ek, err := resolveEnrollmentKey(EnrollmentChoices{}, in, true, &stderr)
	if err != nil {
		t.Fatalf("resolve: %v\nstderr=%s", err, stderr.String())
	}
	if ek.KeyPath != good {
		t.Errorf("KeyPath=%q, want %q (the corrected path)", ek.KeyPath, good)
	}
	if !pub.Equal(ek.PublicKey) {
		t.Errorf("PublicKey mismatch")
	}
	// The "does not exist" line must have appeared so the user
	// knew their typo was caught.
	if !strings.Contains(stderr.String(), "does not exist") {
		t.Errorf("expected 'does not exist' feedback for typo'd path; got %q", stderr.String())
	}
}

// TestResolveAutoManualPathCreateNew — user picks manual-entry and
// types a path that doesn't exist; CLI offers to create a fresh
// ed25519 there (the "drop-in for ssh-keygen" flow). User accepts.
// Resulting EnrollmentKey has privKey set and persistPath pointing
// at the user-chosen location — same shape as --new-key, just at
// an arbitrary path.
func TestResolveAutoManualPathCreateNew(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	// Decoy in ~/.ssh so the picker has a list to render.
	_, decoyPriv, _ := ed25519.GenerateKey(nil)
	writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", decoyPriv)

	// Pick "2" (enter another path), type a non-existent path
	// whose parent (home/.ssh) DOES exist, accept "create here?".
	target := filepath.Join(home, ".ssh", "id_ed25519-txco")
	in := strings.NewReader("2\n" + target + "\ny\n")
	var stderr bytes.Buffer
	ek, err := resolveEnrollmentKey(EnrollmentChoices{Label: "txco@laptop"}, in, true, &stderr)
	if err != nil {
		t.Fatalf("resolve: %v\nstderr=%s", err, stderr.String())
	}
	if ek.privKey == nil {
		t.Errorf("create-new path should set privKey for later PersistFreshKey")
	}
	if ek.persistPath != target {
		t.Errorf("persistPath=%q, want %q", ek.persistPath, target)
	}
	if ek.KeyPath != target {
		t.Errorf("KeyPath=%q, want %q", ek.KeyPath, target)
	}
	// Persisting writes both the OpenSSH PEM and the .pub sidecar.
	if err := ek.PersistFreshKey("txco@laptop"); err != nil {
		t.Fatalf("PersistFreshKey: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("expected private key at %q: %v", target, err)
	}
	if _, err := os.Stat(target + ".pub"); err != nil {
		t.Errorf("expected .pub sidecar at %q.pub: %v", target, err)
	}
}

// TestResolveAutoManualPathDeclineCreate — user types a path that
// doesn't exist but declines the create prompt (typed "n"). The
// picker should re-prompt for a path (loop), not silently fall
// through to fresh keygen under $TXCO_HOME.
func TestResolveAutoManualPathDeclineCreate(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	_, decoyPriv, _ := ed25519.GenerateKey(nil)
	writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", decoyPriv)

	// Inputs:
	//   "2"  → pick manual-entry
	//   "/no/such/path" → bad path; parent doesn't exist, re-prompts
	//   "" → Enter at path: prompt → skip
	in := strings.NewReader("2\n/no/such/path\n\n")
	ek, err := resolveEnrollmentKey(EnrollmentChoices{Name: "local"}, in, true, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.privKey == nil {
		t.Errorf("decline-create + Enter-to-skip should fall through to $TXCO_HOME keygen; got %+v", ek)
	}
}

// TestResolveAutoManualPathEnterToSkip — user picks manual-entry
// but hits Enter (empty input). That's the escape hatch back to
// fresh keygen.
func TestResolveAutoManualPathEnterToSkip(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", priv)

	// Inputs: pick "2" (manual path), then Enter (skip).
	in := strings.NewReader("2\n\n")
	ek, err := resolveEnrollmentKey(EnrollmentChoices{Name: "local"}, in, true, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.privKey == nil {
		t.Errorf("Enter at manual-path prompt should fall through to keygen; got %+v", ek)
	}
}

// TestResolveAutoManualPath — user picks the "enter another path"
// option and types an arbitrary path. Confirms the manual-entry
// branch reaches the same load + confirm machinery.
func TestResolveAutoManualPath(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	// Put a key OUTSIDE ~/.ssh/ so it's not in the auto-listed set.
	pub, priv, _ := ed25519.GenerateKey(nil)
	outside := writeOpenSSHKey(t, filepath.Join(home, "elsewhere"), "alt", priv)
	// Also put one IN ~/.ssh/ so the picker has a list to render.
	_, decoyPriv, _ := ed25519.GenerateKey(nil)
	writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", decoyPriv)

	// Candidates list will have [1] id_ed25519, [2] enter path, [3] skip.
	in := strings.NewReader("2\n" + outside + "\ny\n")
	ek, err := resolveEnrollmentKey(EnrollmentChoices{}, in, true, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.KeyPath != outside {
		t.Errorf("KeyPath=%q, want %q", ek.KeyPath, outside)
	}
	if !pub.Equal(ek.PublicKey) {
		t.Errorf("PublicKey mismatch (should be the manually-entered key)")
	}
}

// TestResolveAutoNonTTYNoAgentFallsToFresh — CI-style: no agent, no
// TTY → don't surprise the pipeline by binding ~/.ssh/id_ed25519
// even if it exists. Generate a fresh key.
func TestResolveAutoNonTTYNoAgentFallsToFresh(t *testing.T) {
	withHome(t)
	withoutAgent(t)
	home := withFakeHome(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	writeOpenSSHKey(t, filepath.Join(home, ".ssh"), "id_ed25519", priv)

	ek, err := resolveEnrollmentKey(EnrollmentChoices{Name: "local"},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.privKey == nil || ek.KeyPath == "" {
		t.Errorf("non-TTY auto path should generate a fresh key; got %+v", ek)
	}
}

// TestResolveNoSSHAgentDeclines — --no-ssh-agent forces the auto
// path to skip the agent even when one is reachable.
func TestResolveNoSSHAgentDeclines(t *testing.T) {
	withHome(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	withAgent(t, priv)
	withFakeHome(t) // no ~/.ssh/id_ed25519 set up

	ek, err := resolveEnrollmentKey(EnrollmentChoices{NoSSHAgent: true, Name: "local"},
		strings.NewReader(""), false, new(bytes.Buffer))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ek.KeySource == SourceSSHAgent {
		t.Errorf("--no-ssh-agent should not pick agent backend; got %+v", ek)
	}
}
