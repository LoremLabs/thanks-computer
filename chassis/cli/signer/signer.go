// Package signer is the CLI-side RFC 9421 signing surface. It is
// deliberately distinct from chassis/auth/signature/ — that package
// wraps yaronf/httpsign for both signing and verifying, and httpsign
// requires a concrete ed25519.PrivateKey at construction time. We need
// to plug in alternative backends (ssh-agent today, hardware tokens
// later), so the outbound side owns its own canonicalization here.
//
// The chassis-side verifier (chassis/auth/signature/verifier.go) does
// NOT use anything from this package — it keeps using httpsign. The
// only contract between us and the server is the wire format: same
// covered components, same Signature-Input parameters, same Ed25519
// signature bytes.
package signer

import (
	"crypto/ed25519"
	"net/http"
)

// Signer signs an outgoing *http.Request in place. After Sign returns,
// the request carries Content-Digest, Signature-Input, and Signature
// headers in the same wire format the chassis verifier expects —
// regardless of whether the signing key lives in process memory, in
// an ssh-agent, or on a hardware token.
type Signer interface {
	// KeyID is the RFC 9421 keyid value the chassis registry uses
	// to look up the public key for verification. Always non-empty
	// for a usable Signer.
	KeyID() string

	// PublicKey is the raw 32-byte Ed25519 public key for this
	// signer, useful for fingerprint display and meta cross-checks.
	PublicKey() ed25519.PublicKey

	// Sign sets the three required headers on req. body is the
	// already-marshaled request body (nil for GETs / empty bodies).
	// Implementations must:
	//   1. compute Content-Digest from body and set the header,
	//   2. compute the RFC 9421 signature base for the covered
	//      components + chosen params,
	//   3. sign the base bytes via the backend (raw ed25519, agent, …),
	//   4. set Signature-Input + Signature headers.
	Sign(req *http.Request, body []byte) error
}
