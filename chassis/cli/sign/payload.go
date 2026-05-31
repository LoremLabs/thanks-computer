// Package sign implements txco's native ed25519 package-signature format: a
// small OCI artifact, discovered by the cosign tag convention
// (sha256:<hex> → sha256-<hex>.sig), whose single layer is the exact signed
// payload bytes and whose annotations carry the ed25519 signature, key id, and
// public key. Verification checks the signature over the STORED payload bytes
// and then binds the payload to the pulled manifest digest + repository, so a
// signature cannot be transplanted onto different content or a different name.
//
// This is deliberately NOT cosign-compatible — it is self-contained (stdlib
// crypto/ed25519, no external binary, no sigstore deps). Trust roots live in
// the workspace txco.yaml; this package only produces and checks the artifact,
// never the trust policy or the registry I/O (that's the CLI + source layers).
package sign

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
)

// OCI media types + annotation keys for the signature artifact.
const (
	MediaTypeSignaturePayload = "application/vnd.thanks.computer.signature.payload.v1alpha1+json"
	ArtifactTypeSignature     = "application/vnd.thanks.computer.signature.v1alpha1"

	AnnotationSignature = "computer.thanks.signature"        // base64(std) ed25519 signature
	AnnotationKeyID     = "computer.thanks.signature.keyid"  // SHA256:… fingerprint
	AnnotationPubKey    = "computer.thanks.signature.pubkey" // base64(std) raw 32-byte pubkey
)

// Payload is the exact JSON that gets signed and stored as the artifact's
// single layer. Field declaration order IS the wire order (encoding/json
// marshals struct fields in declaration order), so the bytes are deterministic.
// Verification trusts only Repository + Digest (the transplant-resistant
// binding); Spec and SignedAt are audit metadata.
type Payload struct {
	Spec       string `json:"spec"`       // the publish --to ref the signer used (audit only)
	Repository string `json:"repository"` // host/ns/name, no tag/digest — binds identity
	Digest     string `json:"digest"`     // sha256:… of the package manifest — binds content
	KeyID      string `json:"keyId"`      // signer.Fingerprint(pub)
	SignedAt   string `json:"signedAt"`   // RFC3339 UTC
}

// Bytes returns the canonical signed bytes for the payload.
func (p Payload) Bytes() ([]byte, error) { return json.Marshal(p) }

// DigestToSigTag maps a manifest digest to its signature tag, mirroring
// cosign's convention: sha256:<hex> → sha256-<hex>.sig. Works on any registry
// (no Referrers API needed). A digest without an "<algo>:" form gets a bare
// ".sig" suffix as a best effort.
func DigestToSigTag(digest string) string {
	if i := strings.IndexByte(digest, ':'); i >= 0 {
		return digest[:i] + "-" + digest[i+1:] + ".sig"
	}
	return digest + ".sig"
}

// KeyIDForPub is the single source of truth for a public key's id: the
// ssh-keygen SHA256 fingerprint (matches `ssh-keygen -lf` / `ssh-add -l`).
func KeyIDForPub(pub ed25519.PublicKey) string { return signer.Fingerprint(pub) }

// ParseTrustedKey accepts a trusted ed25519 public key as any of: an
// authorized_keys line ("ssh-ed25519 AAAA… [comment]"), a path to a .pub file
// containing that line, or a bare base64(std) raw 32-byte key.
func ParseTrustedKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty key")
	}
	if strings.HasPrefix(s, "ssh-") {
		return parseSSHLine(s)
	}
	// A path to a .pub file? (Stat fails for an ssh line or base64 blob.)
	if fi, err := os.Stat(s); err == nil && fi.Mode().IsRegular() {
		b, err := os.ReadFile(s)
		if err != nil {
			return nil, fmt.Errorf("read key file %q: %w", s, err)
		}
		return ParseTrustedKey(string(b))
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("key is neither an ssh-ed25519 line, a .pub path, nor base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("ed25519 public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

func parseSSHLine(s string) (ed25519.PublicKey, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}
	ck, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return nil, fmt.Errorf("unsupported ssh key type")
	}
	ed, ok := ck.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("ssh key is not ed25519")
	}
	return ed, nil
}
