// Package signature is the chassis-owned wrapper around RFC 9421 HTTP
// message signatures. It exposes a small Signer / Verifier interface so
// the rest of the chassis (and CLI) never imports the underlying library
// directly — that keeps the surface area we own small and gives us a
// swap point if we ever inline-replace the implementation.
package signature

import (
	"crypto/ed25519"
	"net/http"
	"time"

	"github.com/yaronf/httpsign"
)

// signedComponents is the fixed list of message components every chassis
// signature covers. We deliberately avoid @target-uri because it
// includes the scheme (https://…) which a TLS-terminating proxy
// rewrites between client and chassis — `https://chassis.example.com/x`
// on the wire becomes `http://localhost:8081/x` on the backend, and
// the signature stops verifying through no fault of the request.
//
// Instead we cover @method @path @query @authority + content-digest:
//   - @method: request semantics (POST vs GET)
//   - @path / @query: exact request line (admin endpoints carry
//     authorization-sensitive args in the query string, e.g.
//     `GET /v1/ops?stack=<prefix>`, so it stays signed)
//   - @authority: the host header — pins the intended chassis
//   - content-digest: the body
// TLS provides the channel-integrity property that signing the scheme
// would otherwise imply.
var signedComponents = httpsign.Headers("@method", "@path", "@query", "@authority", "content-digest")

// signatureName is the label we put inside Signature-Input. RFC 9421
// allows multiple labelled signatures per request; in v1 we always use
// the single name "sig1".
const signatureName = "sig1"

// ActorPrivateKey is what a CLI caller uses to sign outgoing requests.
type ActorPrivateKey struct {
	KeyID      string
	PrivateKey ed25519.PrivateKey
}

// VerifiedSignature is what the verifier returns on success.
type VerifiedSignature struct {
	KeyID     string
	Created   time.Time
	Nonce     string
	Algorithm string
}

// PublicKeyResolver looks up a public key by its keyID and returns its
// Ed25519 public key. The caller (typically the auth middleware) is
// responsible for handling unknown / revoked keys; returning a non-nil
// error short-circuits Verify.
type PublicKeyResolver func(keyID string) (ed25519.PublicKey, error)

// VerifyOptions tunes the verifier's policy. None of these are part of
// RFC 9421 itself — they're chassis policy enforced via library hooks.
type VerifyOptions struct {
	// NotOlderThan rejects signatures whose `created` parameter is older
	// than this. We use it for clock-skew enforcement (5min in v1).
	NotOlderThan time.Duration

	// NonceCheck is called once per request to enforce replay defense.
	// It receives the (keyID, nonce) pair. Returning a non-nil error
	// rejects the request as a replay.
	NonceCheck func(keyID, nonce string) error

	// SkipBodyDigest skips the final body-vs-Content-Digest validation,
	// which buffers the ENTIRE body (the library helper reads it all to
	// hash it). Set it only for streaming routes whose handler performs
	// its own byte-level verification — e.g. the blob endpoint, which
	// hashes the stream and refuses a body whose sha256 differs from the
	// {hash} in the (signature-covered) request path. The Content-Digest
	// HEADER itself remains covered by the signature either way.
	SkipBodyDigest bool
}

// requestExtractor is a tiny indirection so tests can stub the underlying
// library's Details extraction. Production wires it to httpsign.RequestDetails.
type requestExtractor func(name string, req *http.Request) (*httpsign.MessageDetails, error)
