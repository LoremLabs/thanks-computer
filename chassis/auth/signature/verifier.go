package signature

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/yaronf/httpsign"
)

// Verifier validates an incoming *http.Request's RFC 9421 signature.
// On success it returns the resolved keyID + signing metadata so the
// caller can attach them to the request's auth context. Errors are
// classified by the well-known auth-error codes from the design doc.
type Verifier interface {
	Verify(req *http.Request, resolve PublicKeyResolver, opts VerifyOptions) (*VerifiedSignature, error)
}

// NewVerifier returns the default RFC 9421 verifier with the chassis's
// fixed covered components.
func NewVerifier() Verifier {
	return &defaultVerifier{extract: httpsign.RequestDetails}
}

type defaultVerifier struct {
	extract requestExtractor
}

// Verify runs the full verification pipeline:
//  1. parse Signature-Input / Signature headers
//  2. resolve keyID -> public key (caller-provided)
//  3. enforce clock-skew (NotOlderThan)
//  4. enforce replay defense (NonceCheck)
//  5. verify the signature itself (which also re-canonicalizes per RFC 9421)
//  6. validate Content-Digest against the body
//
// Order matters: cheap rejections come first so a request with bad
// metadata never reaches the signature math.
func (v *defaultVerifier) Verify(req *http.Request, resolve PublicKeyResolver, opts VerifyOptions) (*VerifiedSignature, error) {
	details, err := v.extract(signatureName, req)
	if err != nil {
		return nil, &AuthError{Code: ErrMissingSignatureHeaders, Cause: err}
	}
	if details == nil || details.KeyID == nil || *details.KeyID == "" {
		return nil, &AuthError{Code: ErrMissingSignatureHeaders, Cause: errors.New("no keyid in signature input")}
	}
	keyID := *details.KeyID

	if opts.NotOlderThan > 0 {
		if details.Created == nil {
			return nil, &AuthError{Code: ErrTimestampOutOfRange, Cause: errors.New("missing created parameter")}
		}
		age := time.Since(*details.Created)
		if age > opts.NotOlderThan || age < -opts.NotOlderThan {
			return nil, &AuthError{Code: ErrTimestampOutOfRange,
				Cause: fmt.Errorf("created %s is %s away from now", details.Created.UTC().Format(time.RFC3339), age)}
		}
	}

	if opts.NonceCheck != nil {
		if details.Nonce == nil || *details.Nonce == "" {
			return nil, &AuthError{Code: ErrMissingSignatureHeaders, Cause: errors.New("missing nonce")}
		}
		if err := opts.NonceCheck(keyID, *details.Nonce); err != nil {
			return nil, &AuthError{Code: ErrNonceReplay, Cause: err}
		}
	}

	pubKey, err := resolve(keyID)
	if err != nil {
		// Resolver decides between unknown/revoked. Pass the raw error
		// through; middleware maps it to the right code.
		return nil, err
	}

	cfg := httpsign.NewVerifyConfig().
		SetKeyID(keyID).
		SetAllowedAlgs([]string{"ed25519"}).
		SetVerifyCreated(opts.NotOlderThan > 0)
	if opts.NotOlderThan > 0 {
		cfg.SetNotOlderThan(opts.NotOlderThan)
	}

	hverifier, err := httpsign.NewEd25519Verifier(pubKey, cfg, signedComponents)
	if err != nil {
		return nil, &AuthError{Code: ErrInvalidSignature, Cause: err}
	}
	if err := httpsign.VerifyRequest(signatureName, *hverifier, req); err != nil {
		return nil, &AuthError{Code: ErrInvalidSignature, Cause: err}
	}

	// Validate Content-Digest. The library's helper reads the body to
	// hash it; afterwards we put it back so the handler can still
	// consume it. Streaming routes (multi-GB blob uploads) opt out via
	// SkipBodyDigest and do their own hash-while-streaming verification.
	if cd := req.Header.Values("Content-Digest"); len(cd) > 0 && !opts.SkipBodyDigest {
		body := req.Body
		if err := httpsign.ValidateContentDigestHeader(cd, &body, []string{httpsign.DigestSha256}); err != nil {
			return nil, &AuthError{Code: ErrInvalidContentDigest, Cause: err}
		}
		req.Body = body
	}

	out := &VerifiedSignature{
		KeyID:     keyID,
		Algorithm: "ed25519",
	}
	if details.Created != nil {
		out.Created = *details.Created
	}
	if details.Nonce != nil {
		out.Nonce = *details.Nonce
	}
	return out, nil
}

// AuthError maps verification failures to the well-known error codes
// defined in internal docs/todo-chassis-auth-signed-actor-requests.md. Middleware
// converts these into the response JSON.
type AuthError struct {
	Code  string
	Cause error
}

func (e *AuthError) Error() string {
	if e.Cause == nil {
		return e.Code
	}
	return e.Code + ": " + e.Cause.Error()
}

func (e *AuthError) Unwrap() error { return e.Cause }

// Standard error codes — keep in sync with the design doc and middleware.
const (
	ErrMissingSignatureHeaders = "missing_signature_headers"
	ErrUnknownKey              = "unknown_key"
	ErrRevokedKey              = "revoked_key"
	ErrInvalidContentDigest    = "invalid_content_digest"
	ErrInvalidSignature        = "invalid_signature"
	ErrTimestampOutOfRange     = "timestamp_out_of_range"
	ErrNonceReplay             = "nonce_replay"
	ErrActorRevoked            = "actor_revoked"
	ErrCapabilityDenied        = "capability_denied"
)
