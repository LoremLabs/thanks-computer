package signer

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// inMemoryAgent returns an x/crypto/ssh/agent.Agent backed by a
// fresh ssh-agent.NewKeyring(). Tests use this in place of dialing
// $SSH_AUTH_SOCK so they don't depend on the host environment.
func inMemoryAgent(t *testing.T, priv ed25519.PrivateKey, comment string) agent.Agent {
	t.Helper()
	a := agent.NewKeyring()
	if err := a.Add(agent.AddedKey{
		PrivateKey: priv,
		Comment:    comment,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	return a
}

// TestAgentSignerRoundtrip — happy path: in-memory agent holds the
// enrolled key; AgentSigner signs; the chassis-side httpsign
// verifier accepts. Proves the canonical base + signature wrapping
// match across the agent boundary.
func TestAgentSignerRoundtrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	a := inMemoryAgent(t, priv, "test@host")

	s, err := NewAgentSignerFromAgent("key_agent", pub, a)
	if err != nil {
		t.Fatalf("NewAgentSignerFromAgent: %v", err)
	}
	if got := s.KeyID(); got != "key_agent" {
		t.Errorf("KeyID()=%q, want key_agent", got)
	}
	if !pub.Equal(s.PublicKey()) {
		t.Errorf("PublicKey() mismatch")
	}

	req := mustReq(t, "POST", "https://chassis/v1/ops/import", []byte(`{"ops":[]}`))
	if err := s.Sign(req, []byte(`{"ops":[]}`)); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := verifyRequest(t, req, pub); err != nil {
		t.Fatalf("verify after agent sign: %v", err)
	}
}

// TestAgentSignerKeyMismatch — agent holds a different key than the
// caller enrolled. Construction must fail with ErrKeyNotInAgent
// (typed sentinel) so the CLI can suggest `ssh-add ~/.ssh/id_ed25519`.
func TestAgentSignerKeyMismatch(t *testing.T) {
	pubA, _, _ := ed25519.GenerateKey(nil)
	_, privB, _ := ed25519.GenerateKey(nil)
	a := inMemoryAgent(t, privB, "wrong-key@host")

	_, err := NewAgentSignerFromAgent("key_agent", pubA, a)
	if !errors.Is(err, ErrKeyNotInAgent) {
		t.Fatalf("got %v, want ErrKeyNotInAgent", err)
	}
	// Error message includes the fingerprint of the expected key so
	// the user can see which one to add.
	if !strings.Contains(err.Error(), "SHA256:") {
		t.Errorf("error %q should mention expected key fingerprint", err)
	}
}

// TestAgentSignerMultipleKeysPicksRight — agent has several
// Ed25519 keys; AgentSigner picks the one matching wantPub by
// fingerprint, not by order.
func TestAgentSignerMultipleKeysPicksRight(t *testing.T) {
	_, decoyPriv, _ := ed25519.GenerateKey(nil)
	wantPub, wantPriv, _ := ed25519.GenerateKey(nil)

	a := agent.NewKeyring()
	_ = a.Add(agent.AddedKey{PrivateKey: decoyPriv, Comment: "decoy"})
	_ = a.Add(agent.AddedKey{PrivateKey: wantPriv, Comment: "target"})

	s, err := NewAgentSignerFromAgent("key_agent", wantPub, a)
	if err != nil {
		t.Fatalf("NewAgentSignerFromAgent: %v", err)
	}
	req := mustReq(t, "GET", "https://chassis/auth/whoami", nil)
	if err := s.Sign(req, nil); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := verifyRequest(t, req, wantPub); err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// fakeAgent is a minimal agent.Agent that returns whatever we tell it
// to from Sign. Used to exercise the format/length assertions in
// AgentSigner without finding a real misbehaving agent.
type fakeAgent struct {
	keys []*agent.Key
	sig  *ssh.Signature
	err  error
}

func (f *fakeAgent) List() ([]*agent.Key, error)                       { return f.keys, nil }
func (f *fakeAgent) Sign(_ ssh.PublicKey, _ []byte) (*ssh.Signature, error) {
	return f.sig, f.err
}
func (f *fakeAgent) Add(agent.AddedKey) error            { return errors.New("unsupported") }
func (f *fakeAgent) Remove(ssh.PublicKey) error          { return errors.New("unsupported") }
func (f *fakeAgent) RemoveAll() error                    { return errors.New("unsupported") }
func (f *fakeAgent) Lock([]byte) error                   { return errors.New("unsupported") }
func (f *fakeAgent) Unlock([]byte) error                 { return errors.New("unsupported") }
func (f *fakeAgent) Signers() ([]ssh.Signer, error)      { return nil, errors.New("unsupported") }

// newFakeAgentWithKey builds a fakeAgent that lists the given pubkey
// as an Ed25519 entry but returns a caller-specified signature when
// Sign is invoked.
func newFakeAgentWithKey(t *testing.T, pub ed25519.PublicKey, returnSig *ssh.Signature) *fakeAgent {
	t.Helper()
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeAgent{
		keys: []*agent.Key{{
			Format:  sshPub.Type(),
			Blob:    sshPub.Marshal(),
			Comment: "fake",
		}},
		sig: returnSig,
	}
}

// TestAgentSignerRejectsWrongFormat — if the agent returns a
// signature with a non-ssh-ed25519 Format (e.g. it silently fell back
// to a different key in the keyring), the signer must refuse rather
// than ship something the verifier will reject anyway. Loud failure
// here is much better than a confusing "401 invalid_signature" later.
func TestAgentSignerRejectsWrongFormat(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	bogus := &ssh.Signature{
		Format: "rsa-sha2-256",
		Blob:   make([]byte, 256),
	}
	a := newFakeAgentWithKey(t, pub, bogus)

	s, err := NewAgentSignerFromAgent("key_agent", pub, a)
	if err != nil {
		t.Fatalf("NewAgentSignerFromAgent: %v", err)
	}
	req := mustReq(t, "GET", "https://chassis/auth/whoami", nil)
	err = s.Sign(req, nil)
	if err == nil {
		t.Fatal("expected error on non-ssh-ed25519 format")
	}
	if !strings.Contains(err.Error(), "format") {
		t.Errorf("error %q should mention 'format'", err)
	}
}

// TestAgentSignerRejectsWrongBlobLen — a signature blob that isn't
// exactly 64 bytes is by definition not a raw Ed25519 signature
// (the wire format the chassis expects). Refuse before the verifier
// has to.
func TestAgentSignerRejectsWrongBlobLen(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	bogus := &ssh.Signature{
		Format: ssh.KeyAlgoED25519,
		Blob:   make([]byte, 63), // off-by-one
	}
	a := newFakeAgentWithKey(t, pub, bogus)

	s, err := NewAgentSignerFromAgent("key_agent", pub, a)
	if err != nil {
		t.Fatalf("NewAgentSignerFromAgent: %v", err)
	}
	req := mustReq(t, "GET", "https://chassis/auth/whoami", nil)
	err = s.Sign(req, nil)
	if err == nil {
		t.Fatal("expected error on short signature blob")
	}
	if !strings.Contains(err.Error(), "length") {
		t.Errorf("error %q should mention 'length'", err)
	}
}
