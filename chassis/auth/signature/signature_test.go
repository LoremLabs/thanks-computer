package signature

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Wraps an in-memory request body so it satisfies io.ReadCloser.
type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	return pub, priv
}

func mustReq(t *testing.T, method, target string, body []byte) *http.Request {
	t.Helper()
	r, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		r.Body = readCloser{bytes.NewReader(body)}
		r.ContentLength = int64(len(body))
	}
	return r
}

// TestSignAndVerifyRoundtrip is the happy path: sign with key K, verify
// with K's public side, succeed and surface the keyID + nonce.
func TestSignAndVerifyRoundtrip(t *testing.T) {
	pub, priv := mustKeypair(t)
	signer := NewSigner()
	verifier := NewVerifier()

	body := []byte(`{"ops":[{"stack":"x","scope":1,"name":"y","txcl":"EXEC \"http://x\""}]}`)
	req := mustReq(t, "POST", "https://chassis.example.com/v1/ops/import?dry=0", body)
	if err := signer.Sign(req, ActorPrivateKey{KeyID: "key_test", PrivateKey: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	if got := req.Header.Get("Signature"); got == "" {
		t.Fatal("Signature header not set after Sign")
	}
	if got := req.Header.Get("Signature-Input"); got == "" {
		t.Fatal("Signature-Input header not set after Sign")
	}
	if got := req.Header.Get("Content-Digest"); got == "" {
		t.Fatal("Content-Digest header not set after Sign")
	}

	resolve := func(id string) (ed25519.PublicKey, error) {
		if id != "key_test" {
			return nil, errors.New("unknown key")
		}
		return pub, nil
	}
	v, err := verifier.Verify(req, resolve, VerifyOptions{NotOlderThan: 5 * time.Minute})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if v.KeyID != "key_test" {
		t.Errorf("got keyID %q, want key_test", v.KeyID)
	}
	if v.Nonce == "" {
		t.Error("expected non-empty nonce on verified result")
	}
	if time.Since(v.Created) > time.Minute {
		t.Errorf("Created %v looks wrong (now is %v)", v.Created, time.Now())
	}
}

// TestVerifyRejectsTamperedBody guarantees content-digest enforcement —
// flipping a byte in the body must invalidate the signature.
func TestVerifyRejectsTamperedBody(t *testing.T) {
	pub, priv := mustKeypair(t)
	signer := NewSigner()
	verifier := NewVerifier()

	body := []byte(`{"v":1}`)
	req := mustReq(t, "POST", "https://x/v1/ops/import", body)
	if err := signer.Sign(req, ActorPrivateKey{KeyID: "k", PrivateKey: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Replace the body with a different payload.
	req.Body = readCloser{bytes.NewReader([]byte(`{"v":2}`))}
	req.ContentLength = 7

	resolve := func(id string) (ed25519.PublicKey, error) { return pub, nil }
	if _, err := verifier.Verify(req, resolve, VerifyOptions{NotOlderThan: 5 * time.Minute}); err == nil {
		t.Fatal("expected verification to fail on tampered body, got nil")
	}
}

// TestVerifyRejectsWrongKey ensures a signature made by key A doesn't
// verify against key B's public side.
func TestVerifyRejectsWrongKey(t *testing.T) {
	_, signingKey := mustKeypair(t)
	otherPub, _ := mustKeypair(t)

	req := mustReq(t, "GET", "https://x/v1/ops", nil)
	if err := NewSigner().Sign(req, ActorPrivateKey{KeyID: "k", PrivateKey: signingKey}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	resolve := func(id string) (ed25519.PublicKey, error) { return otherPub, nil }
	_, err := NewVerifier().Verify(req, resolve, VerifyOptions{NotOlderThan: 5 * time.Minute})
	if err == nil {
		t.Fatal("expected verification to fail with mismatched key")
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != ErrInvalidSignature {
		t.Errorf("got error %v, want code=%s", err, ErrInvalidSignature)
	}
}

// TestVerifyMissingSignatureHeaders surfaces the right error code when
// the request is unsigned.
func TestVerifyMissingSignatureHeaders(t *testing.T) {
	req := mustReq(t, "GET", "https://x/v1/ops", nil)
	resolve := func(string) (ed25519.PublicKey, error) { return nil, nil }
	_, err := NewVerifier().Verify(req, resolve, VerifyOptions{NotOlderThan: 5 * time.Minute})
	if err == nil {
		t.Fatal("expected error for unsigned request")
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != ErrMissingSignatureHeaders {
		t.Errorf("got error %v, want code=%s", err, ErrMissingSignatureHeaders)
	}
}

// TestVerifyResolverError propagates the resolver's failure cleanly.
// The middleware uses this to return ErrUnknownKey vs ErrRevokedKey.
func TestVerifyResolverError(t *testing.T) {
	_, priv := mustKeypair(t)
	req := mustReq(t, "GET", "https://x/v1/ops", nil)
	if err := NewSigner().Sign(req, ActorPrivateKey{KeyID: "k", PrivateKey: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	want := &AuthError{Code: ErrRevokedKey, Cause: errors.New("revoked")}
	resolve := func(string) (ed25519.PublicKey, error) { return nil, want }
	_, err := NewVerifier().Verify(req, resolve, VerifyOptions{NotOlderThan: 5 * time.Minute})
	if !errors.Is(err, want) && err != want {
		t.Errorf("got %v, want resolver error to pass through", err)
	}
}

// TestVerifyNonceCheckFailureIsReplay maps the resolver's nonce error
// onto ErrNonceReplay so the middleware can produce the right code.
func TestVerifyNonceCheckFailureIsReplay(t *testing.T) {
	pub, priv := mustKeypair(t)
	req := mustReq(t, "GET", "https://x/v1/ops", nil)
	if err := NewSigner().Sign(req, ActorPrivateKey{KeyID: "k", PrivateKey: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	resolve := func(string) (ed25519.PublicKey, error) { return pub, nil }
	opts := VerifyOptions{
		NotOlderThan: 5 * time.Minute,
		NonceCheck:   func(keyID, nonce string) error { return errors.New("seen this one") },
	}
	_, err := NewVerifier().Verify(req, resolve, opts)
	if err == nil {
		t.Fatal("expected nonce-replay error")
	}
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Code != ErrNonceReplay {
		t.Errorf("got %v, want code=%s", err, ErrNonceReplay)
	}
}

// TestSignerHandlesEmptyBody ensures GET requests sign cleanly even
// without a body — Content-Digest gets set to the empty-body constant.
func TestSignerHandlesEmptyBody(t *testing.T) {
	_, priv := mustKeypair(t)
	req := mustReq(t, "GET", "https://x/v1/ops?stack=demo", nil)
	if err := NewSigner().Sign(req, ActorPrivateKey{KeyID: "k", PrivateKey: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if got := req.Header.Get("Content-Digest"); got != emptyBodyDigest {
		t.Errorf("got Content-Digest %q, want empty-body sentinel", got)
	}
}

// TestProxyMutatedSchemeStillVerifies guards against re-adding
// `@target-uri` to signedComponents. The client signs an `https://`
// URL; a TLS-terminating proxy forwards plaintext to the chassis,
// where the request's URL.Scheme is `http`. Verification must still
// succeed because we only cover @path/@query/@authority (which the
// proxy preserves), not the scheme that the proxy rewrites.
func TestProxyMutatedSchemeStillVerifies(t *testing.T) {
	pub, priv := mustKeypair(t)
	signed := mustReq(t, "GET", "https://chassis.example.com/v1/ops?stack=x", nil)
	if err := NewSigner().Sign(signed, ActorPrivateKey{KeyID: "k", PrivateKey: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}

	// Simulate the chassis seeing the request after a TLS-terminating
	// proxy: same method, path, query, and Host header, but http://
	// scheme on the backend interface.
	backend := mustReq(t, "GET", "http://chassis.example.com/v1/ops?stack=x", nil)
	// Carry over the signature headers + Host (httptest doesn't keep
	// these across the synthetic request, so do it manually).
	for _, h := range []string{"Signature-Input", "Signature", "Content-Digest"} {
		backend.Header.Set(h, signed.Header.Get(h))
	}
	backend.Host = signed.Host

	resolve := func(string) (ed25519.PublicKey, error) { return pub, nil }
	if _, err := NewVerifier().Verify(backend, resolve, VerifyOptions{NotOlderThan: 5 * time.Minute}); err != nil {
		t.Fatalf("expected verification to succeed across proxy scheme rewrite; got %v", err)
	}
}

// TestSignerCoverageIncludesQueryString proves we sign over the
// @query component — so flipping a query param post-sign invalidates
// the signature. This is the security guarantee that lets
// `GET /v1/ops?stack=<prefix>` carry authorization-sensitive args.
func TestSignerCoverageIncludesQueryString(t *testing.T) {
	pub, priv := mustKeypair(t)
	req := mustReq(t, "GET", "https://x/v1/ops?stack=alice", nil)
	if err := NewSigner().Sign(req, ActorPrivateKey{KeyID: "k", PrivateKey: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Tamper with the query string after signing.
	req.URL.RawQuery = "stack=bob"

	resolve := func(string) (ed25519.PublicKey, error) { return pub, nil }
	_, err := NewVerifier().Verify(req, resolve, VerifyOptions{NotOlderThan: 5 * time.Minute})
	if err == nil {
		t.Fatal("expected verification to fail after query-string tamper")
	}
}

// TestVerifyAcceptsHttpTestServer is a small belt-and-suspenders check
// that the wrapper plays nicely with httptest.NewServer (which is the
// shape every middleware test will use).
func TestVerifyAcceptsHttpTestServer(t *testing.T) {
	pub, priv := mustKeypair(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Body must be drained before Verify runs (the wrapper
		// re-assigns r.Body during digest validation).
		resolve := func(string) (ed25519.PublicKey, error) { return pub, nil }
		if _, err := NewVerifier().Verify(r, resolve, VerifyOptions{NotOlderThan: 5 * time.Minute}); err != nil {
			t.Errorf("Verify: %v", err)
			http.Error(w, err.Error(), 401)
			return
		}
		// Echo the body to confirm it's still readable post-verify.
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	body := []byte(`{"ping":"ok"}`)
	req, _ := http.NewRequest("POST", srv.URL+"/echo", strings.NewReader(string(body)))
	req.Body = readCloser{bytes.NewReader(body)}
	req.ContentLength = int64(len(body))
	if err := NewSigner().Sign(req, ActorPrivateKey{KeyID: "k", PrivateKey: priv}); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		out, _ := io.ReadAll(resp.Body)
		t.Errorf("status=%d body=%s", resp.StatusCode, out)
	}
}
