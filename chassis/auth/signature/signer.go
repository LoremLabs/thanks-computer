package signature

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/yaronf/httpsign"
)

// Signer signs an outgoing *http.Request in place. After Sign returns,
// the request carries `Content-Digest`, `Signature-Input`, and
// `Signature` headers.
//
// Note: this is the SERVER-SIDE signer surface (used in chassis-side
// tests that need to mint a signed request, e.g. integration suites
// that exercise the verifier). The CLI's signing path lives in
// chassis/cli/signer/ — it owns its own RFC 9421 canonicalizer so
// it can plug in non-key backends like ssh-agent. Both paths produce
// byte-identical wire output verified by the same chassis verifier.
type Signer interface {
	Sign(req *http.Request, key ActorPrivateKey) error
}

// NewSigner returns the default RFC 9421 signer. It always covers the
// component set declared in signedComponents (@method, @path, @query,
// @authority, content-digest), uses ed25519, and embeds a fresh nonce
// + the current `created` timestamp on every signature.
func NewSigner() Signer {
	return &defaultSigner{}
}

type defaultSigner struct{}

func (s *defaultSigner) Sign(req *http.Request, key ActorPrivateKey) error {
	if key.KeyID == "" {
		return errors.New("signature: missing keyID")
	}
	if len(key.PrivateKey) != ed25519PrivateKeySize {
		return fmt.Errorf("signature: bad ed25519 private key length %d", len(key.PrivateKey))
	}

	// Compute Content-Digest. The library wants a *io.ReadCloser so it
	// can read and rewind. For requests with no body, we still need a
	// digest header so verification is well-defined.
	if err := setContentDigest(req); err != nil {
		return fmt.Errorf("signature: content-digest: %w", err)
	}

	nonce, err := newNonce()
	if err != nil {
		return fmt.Errorf("signature: nonce: %w", err)
	}

	cfg := httpsign.NewSignConfig().
		SetKeyID(key.KeyID).
		SetNonce(nonce).
		SignCreated(true)

	hsigner, err := httpsign.NewEd25519Signer(key.PrivateKey, cfg, signedComponents)
	if err != nil {
		return fmt.Errorf("signature: build signer: %w", err)
	}

	sigInput, sig, err := httpsign.SignRequest(signatureName, *hsigner, req)
	if err != nil {
		return fmt.Errorf("signature: sign request: %w", err)
	}
	req.Header.Set("Signature-Input", sigInput)
	req.Header.Set("Signature", sig)
	return nil
}

// emptyBodyDigest is the sha-256 digest of zero bytes, base64-encoded
// per RFC 9530. Pinning it as a constant means we don't have to hash
// nothing on every empty-body request.
const emptyBodyDigest = "sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:"

func setContentDigest(req *http.Request) error {
	if req.Body == nil || req.Body == http.NoBody {
		req.Header.Set("Content-Digest", emptyBodyDigest)
		return nil
	}

	// We need to read the body to digest it, then put it back so the
	// request can still be sent. The library's GenerateContentDigestHeader
	// wraps the body for us.
	body := req.Body
	digest, err := httpsign.GenerateContentDigestHeader(&body, []string{httpsign.DigestSha256})
	if err != nil {
		return err
	}
	req.Body = body
	req.Header.Set("Content-Digest", digest)
	return nil
}

// newNonce produces a 16-byte random nonce, encoded as URL-safe base64
// without padding. Per RFC 9421 the nonce is an opaque structured
// string; this gives us 128 bits of entropy in 22 ASCII characters.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const ed25519PrivateKeySize = 64 // ed25519.PrivateKey size
