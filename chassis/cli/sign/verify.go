package sign

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
)

// TrustedKey is a public key the consumer trusts, optionally scoped to a
// registry host. KeyID is the ssh fingerprint used for matching.
type TrustedKey struct {
	Name     string
	Pub      ed25519.PublicKey
	KeyID    string
	Registry string // optional host scope; empty = trusted on any registry
}

// Verdict is the result of checking a package's signature. A malformed, absent,
// or untrusted signature is a Verdict (not a Go error) — Go errors are reserved
// for the caller's transport layer.
type Verdict struct {
	Signed   bool   `json:"signed"`
	KeyID    string `json:"keyId,omitempty"`
	Trusted  bool   `json:"trusted"`
	Reason   string `json:"reason,omitempty"`
	SignedAt string `json:"signedAt,omitempty"`
}

// VerifyArtifact checks an already-fetched signature artifact against the pulled
// manifest digest + repository and the set of trusted keys. registryHost scopes
// registry-bound trusted keys (pass the pulled registry host; may be empty).
func VerifyArtifact(manifestBytes, layerBytes []byte, ann map[string]string, expectDigest, expectRepository, registryHost string, trusted []TrustedKey) Verdict {
	var p Payload
	if err := json.Unmarshal(layerBytes, &p); err != nil {
		return Verdict{Reason: "signature payload malformed"}
	}
	sigB64, keyID, pubB64 := ann[AnnotationSignature], ann[AnnotationKeyID], ann[AnnotationPubKey]
	if sigB64 == "" || keyID == "" || pubB64 == "" {
		return Verdict{Reason: "signature artifact missing annotations"}
	}
	sig, err1 := base64.StdEncoding.DecodeString(sigB64)
	rawPub, err2 := base64.StdEncoding.DecodeString(pubB64)
	if err1 != nil || err2 != nil || len(rawPub) != ed25519.PublicKeySize {
		return Verdict{Reason: "signature artifact malformed"}
	}
	pub := ed25519.PublicKey(rawPub)
	if KeyIDForPub(pub) != keyID {
		return Verdict{KeyID: keyID, Reason: "key id does not match embedded public key"}
	}
	if !ed25519.Verify(pub, layerBytes, sig) {
		return Verdict{KeyID: keyID, Reason: "signature does not verify over payload"}
	}
	// A cryptographically valid signature exists from here on (Signed = true).
	if p.Digest != expectDigest {
		return Verdict{Signed: true, KeyID: keyID, SignedAt: p.SignedAt, Reason: "signature is for a different digest (transplanted?)"}
	}
	if p.Repository != expectRepository {
		return Verdict{Signed: true, KeyID: keyID, SignedAt: p.SignedAt, Reason: "signature is for a different repository (transplanted?)"}
	}
	for _, t := range trusted {
		if t.KeyID != keyID {
			continue
		}
		if t.Registry != "" && t.Registry != registryHost {
			continue
		}
		return Verdict{Signed: true, Trusted: true, KeyID: keyID, SignedAt: p.SignedAt}
	}
	return Verdict{Signed: true, KeyID: keyID, SignedAt: p.SignedAt, Reason: "signed by untrusted key " + keyID}
}

// String renders a one-line human summary for display.
func (v Verdict) String() string {
	switch {
	case v.Signed && v.Trusted:
		return "verified: signed by " + v.KeyID
	case v.Signed:
		return "signed but untrusted: " + v.Reason
	case v.Reason != "":
		return "unsigned: " + v.Reason
	default:
		return "unsigned"
	}
}
