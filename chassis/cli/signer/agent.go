package signer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// ErrNoAgent signals "$SSH_AUTH_SOCK is unset or the socket can't be
// reached." Surface this as a typed sentinel so the enrollment
// resolver can distinguish "no agent" from "agent reachable but has
// no matching key" — the two cases need different operator
// messages.
var ErrNoAgent = errors.New("ssh-agent not available (SSH_AUTH_SOCK unset or unreachable)")

// ErrKeyNotInAgent signals "agent is reachable but doesn't hold a
// key matching the one we expect." Most commonly: the user logged
// out / ssh-add -D'd between enrollment and the next signed call.
var ErrKeyNotInAgent = errors.New("expected key not present in ssh-agent")

// AgentSigner signs via an ssh-agent. The private key never leaves
// the agent's process (or the hardware token it brokers — Yubikeys,
// Secure Enclave via Secretive, PKCS#11 cards). We only ever send
// the agent the canonical RFC 9421 signature base; the agent returns
// a raw Ed25519 signature blob which we set verbatim in the
// Signature header.
type AgentSigner struct {
	keyID string
	pub   ed25519.PublicKey
	a     agent.Agent
	// sshKey is the agent.Key we matched at construction; cached so
	// every Sign call doesn't re-list the agent and re-fingerprint.
	sshKey *agent.Key
}

// NewAgentSigner dials $SSH_AUTH_SOCK and locates wantPub. Returns
// typed sentinels for the two failure modes that need distinct CLI
// messages: ErrNoAgent (agent unreachable) and ErrKeyNotInAgent
// (agent reachable, doesn't have wantPub).
func NewAgentSigner(keyID string, wantPub ed25519.PublicKey) (*AgentSigner, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, ErrNoAgent
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %q: %v", ErrNoAgent, sock, err)
	}
	// Note: we deliberately don't close conn — the agent.Client keeps
	// it open for the lifetime of the signer. The OS reclaims on
	// process exit.
	return NewAgentSignerFromAgent(keyID, wantPub, agent.NewClient(conn))
}

// NewAgentSignerFromAgent is the testable constructor; production
// callers use NewAgentSigner. Accepts any agent.Agent so unit tests
// can plug in agent.NewKeyring() pre-loaded with the expected key.
func NewAgentSignerFromAgent(keyID string, wantPub ed25519.PublicKey, a agent.Agent) (*AgentSigner, error) {
	if keyID == "" {
		return nil, errors.New("agent signer: empty keyID")
	}
	if len(wantPub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("agent signer: bad public key size %d", len(wantPub))
	}

	keys, err := a.List()
	if err != nil {
		return nil, fmt.Errorf("agent signer: list keys: %w", err)
	}
	wantBlob, err := sshMarshalEd25519(wantPub)
	if err != nil {
		return nil, fmt.Errorf("agent signer: marshal expected pubkey: %w", err)
	}
	for _, k := range keys {
		// Type filter: only Ed25519 keys are candidates. The
		// chassis only registers raw 32-byte ed25519 keys; anything
		// else in the agent is irrelevant to us.
		if k.Format != ssh.KeyAlgoED25519 {
			continue
		}
		if bytes.Equal(k.Marshal(), wantBlob) {
			return &AgentSigner{
				keyID:  keyID,
				pub:    wantPub,
				a:      a,
				sshKey: k,
			}, nil
		}
	}
	return nil, fmt.Errorf("%w: looking for %s", ErrKeyNotInAgent, Fingerprint(wantPub))
}

// KeyID implements Signer.
func (s *AgentSigner) KeyID() string { return s.keyID }

// PublicKey implements Signer.
func (s *AgentSigner) PublicKey() ed25519.PublicKey { return s.pub }

// Sign implements Signer. Same canonical base as FileKeySigner;
// the signing primitive is `agent.Agent.Sign` instead of
// `ed25519.Sign`. We strictly validate the returned signature's
// Format and Blob length before trusting it.
func (s *AgentSigner) Sign(req *http.Request, body []byte) error {
	digest := computeContentDigest(req, body)
	nonce, err := newNonce()
	if err != nil {
		return fmt.Errorf("agent signer: nonce: %w", err)
	}
	params := signParams{KeyID: s.keyID, Created: nowUnix(), Nonce: nonce}
	inputValue := buildSignatureInputValue(params)
	base := buildSignatureBase(req, digest, inputValue)

	sshPub, err := ssh.ParsePublicKey(s.sshKey.Marshal())
	if err != nil {
		return fmt.Errorf("agent signer: re-parse agent pubkey: %w", err)
	}
	sig, err := s.a.Sign(sshPub, base)
	if err != nil {
		return fmt.Errorf("agent signer: agent.Sign: %w", err)
	}

	// Defensive assertions on what the agent gave us. A
	// well-behaved agent only ever returns ssh-ed25519 signatures
	// for an ssh-ed25519 key, but we check explicitly:
	//   - Format != ssh-ed25519 means the agent silently picked a
	//     different key (or returned an RSA-wrapped Ed25519 hybrid).
	//   - Blob length != 64 means the signature isn't a raw Ed25519
	//     output and the chassis verifier would reject it.
	// Both are caller errors — make them loud here, not later.
	if sig.Format != ssh.KeyAlgoED25519 {
		return fmt.Errorf("agent signer: unexpected signature format %q (want %q); agent may be returning the wrong key",
			sig.Format, ssh.KeyAlgoED25519)
	}
	if len(sig.Blob) != ed25519.SignatureSize {
		return fmt.Errorf("agent signer: signature blob length %d (want %d)",
			len(sig.Blob), ed25519.SignatureSize)
	}

	sigB64 := base64.StdEncoding.EncodeToString(sig.Blob)
	req.Header.Set("Signature-Input", signatureLabel+"="+inputValue)
	req.Header.Set("Signature", signatureLabel+"=:"+sigB64+":")
	return nil
}

// sshMarshalEd25519 returns the SSH wire format of an Ed25519 public
// key: ("ssh-ed25519" string, 32-byte raw pubkey string). Same bytes
// agent.Key.Marshal() returns, so we can compare blobs directly to
// find the matching key without re-parsing.
func sshMarshalEd25519(pub ed25519.PublicKey) ([]byte, error) {
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return k.Marshal(), nil
}
