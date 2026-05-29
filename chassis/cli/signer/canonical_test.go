package signer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
)

// signWithCanonicalizer is the per-test helper that walks the full
// outbound signing path manually: compute content digest, build the
// canonical base, sign with raw ed25519, set the headers. This is
// exactly what FileKeySigner/AgentSigner will do in production —
// keeping it inline here proves the canonicalizer is correct
// independent of the backend wrappers.
func signWithCanonicalizer(t *testing.T, req *http.Request, body []byte, keyID string, priv ed25519.PrivateKey) {
	t.Helper()
	digest := computeContentDigest(req, body)
	nonce, err := newNonce()
	if err != nil {
		t.Fatalf("nonce: %v", err)
	}
	params := signParams{KeyID: keyID, Created: time.Now().Unix(), Nonce: nonce}
	inputValue := buildSignatureInputValue(params)
	base := buildSignatureBase(req, digest, inputValue)

	sig := ed25519.Sign(priv, base)
	sigB64 := base64.StdEncoding.EncodeToString(sig)

	req.Header.Set("Signature-Input", signatureLabel+"="+inputValue)
	req.Header.Set("Signature", signatureLabel+"=:"+sigB64+":")
}

// verifyRequest runs the chassis-side verifier against req. Returns
// nil on success, the verifier's error otherwise. We exercise the
// real production verifier so canonical-base drift surfaces here.
func verifyRequest(t *testing.T, req *http.Request, pub ed25519.PublicKey) error {
	t.Helper()
	resolve := func(string) (ed25519.PublicKey, error) { return pub, nil }
	_, err := signature.NewVerifier().Verify(req, resolve, signature.VerifyOptions{
		NotOlderThan: 5 * time.Minute,
	})
	return err
}

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return pub, priv
}

func mustReq(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	return req
}

// TestCanonicalBaseMatchesHttpsign is the drift catcher: our canonical
// base must produce bytes that the chassis-side httpsign verifier
// accepts. If component formatting or @signature-params serialization
// drifts, this test fails immediately.
func TestCanonicalBaseMatchesHttpsign(t *testing.T) {
	pub, priv := mustKeypair(t)
	cases := []struct {
		name string
		req  *http.Request
		body []byte
	}{
		{"GET no body, no query", mustReq(t, "GET", "https://chassis.example.com/v1/ops", nil), nil},
		{"GET with query", mustReq(t, "GET", "https://chassis.example.com/v1/ops?stack=alice&scope=100", nil), nil},
		{"POST with body", mustReq(t, "POST", "https://chassis.example.com/v1/ops/import", []byte(`{"ops":[]}`)), []byte(`{"ops":[]}`)},
		{"POST nested path", mustReq(t, "POST", "https://chassis.example.com/auth/keys/key_01HVABC/revoke", nil), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signWithCanonicalizer(t, tc.req, tc.body, "key_test", priv)
			if err := verifyRequest(t, tc.req, pub); err != nil {
				t.Fatalf("verifier rejected our signature: %v", err)
			}
		})
	}
}

// TestProxyMutatedSchemeStillVerifies is the regression test for the
// `@target-uri` mistake. Client signs an https:// URL; the chassis
// receives an http:// URL behind a TLS terminator. Verification must
// still succeed because @path/@query/@authority survive proxy
// rewriting and @target-uri is no longer in our covered set.
func TestProxyMutatedSchemeStillVerifies(t *testing.T) {
	pub, priv := mustKeypair(t)
	signed := mustReq(t, "GET", "https://chassis.example.com/v1/ops?stack=x", nil)
	signWithCanonicalizer(t, signed, nil, "key_test", priv)

	// Reconstruct what the chassis sees behind a TLS-terminating
	// proxy: same path/query/Host, but http:// on the backend.
	backend := mustReq(t, "GET", "http://chassis.example.com/v1/ops?stack=x", nil)
	for _, h := range []string{"Signature-Input", "Signature", "Content-Digest"} {
		backend.Header.Set(h, signed.Header.Get(h))
	}
	backend.Host = signed.Host

	if err := verifyRequest(t, backend, pub); err != nil {
		t.Fatalf("verifier rejected proxy-mutated request: %v", err)
	}
}

// TestQueryTamperingInvalidates — flipping a query param after
// signing must invalidate the signature. Confirms @query is actually
// covered (the security guarantee for query-bearing endpoints like
// `GET /v1/ops?stack=<prefix>`).
func TestQueryTamperingInvalidates(t *testing.T) {
	pub, priv := mustKeypair(t)
	req := mustReq(t, "GET", "https://x/v1/ops?stack=alice", nil)
	signWithCanonicalizer(t, req, nil, "key_test", priv)
	req.URL.RawQuery = "stack=bob"

	if err := verifyRequest(t, req, pub); err == nil {
		t.Fatal("expected verification failure after query tampering")
	}
}

// TestBodyTamperingInvalidates — content-digest binds the body;
// changing the body without re-signing must fail.
func TestBodyTamperingInvalidates(t *testing.T) {
	pub, priv := mustKeypair(t)
	body := []byte(`{"k":1}`)
	req := mustReq(t, "POST", "https://x/v1/ops/import", body)
	signWithCanonicalizer(t, req, body, "key_test", priv)

	// Tamper: replace the body on the request after signing.
	tampered := []byte(`{"k":2}`)
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	req.ContentLength = int64(len(tampered))
	// Leave the Content-Digest header in place (tampered body now
	// disagrees with it) — verifier should reject.

	if err := verifyRequest(t, req, pub); err == nil {
		t.Fatal("expected verification failure after body tampering")
	}
}

// TestEmptyQueryUsesQuestionMark — RFC 9421 §2.2.7 mandates "?" for
// empty queries. Test it explicitly rather than relying on the
// roundtrip alone.
func TestEmptyQueryUsesQuestionMark(t *testing.T) {
	req := mustReq(t, "GET", "https://x/v1/ops", nil)
	digest := computeContentDigest(req, nil)
	base := buildSignatureBase(req, digest, "()")
	if !strings.Contains(string(base), "\"@query\": ?\n") {
		t.Fatalf("base missing canonical empty-query line:\n%s", base)
	}
}
