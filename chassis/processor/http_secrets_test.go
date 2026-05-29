package processor

import (
	"bytes"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// TestApplySecretOverlaysHeaderRaw covers the raw-substitution case
// for a header: `secrets.headers.x-api-key.secret = NAME` with no
// format → header value is the cleartext.
func TestApplySecretOverlaysHeaderRaw(t *testing.T) {
	refs := []secrets.Ref{{Path: "headers.x-api-key", Secret: "VENDOR_KEY"}}
	var bag secrets.SecretBag
	bag.Set("VENDOR_KEY", []byte("sk_vendor_abc123"))
	headerOverlays := map[string]string{}
	body, err := applySecretOverlays(refs, bag, []byte(`{}`), headerOverlays)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(body) != `{}` {
		t.Errorf("body changed for header-only ref: %s", body)
	}
	if got := headerOverlays["x-api-key"]; got != "sk_vendor_abc123" {
		t.Errorf("header value = %q, want sk_vendor_abc123", got)
	}
}

// TestApplySecretOverlaysHeaderFormatted is the canonical Bearer-token
// case: format = "Bearer {}" + cleartext sk_live_… → "Bearer sk_live_…".
func TestApplySecretOverlaysHeaderFormatted(t *testing.T) {
	refs := []secrets.Ref{{Path: "headers.authorization", Secret: "STRIPE_KEY", Format: "Bearer {}"}}
	var bag secrets.SecretBag
	bag.Set("STRIPE_KEY", []byte("sk_live_abc"))
	headerOverlays := map[string]string{}
	_, err := applySecretOverlays(refs, bag, []byte(`{}`), headerOverlays)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := headerOverlays["authorization"]; got != "Bearer sk_live_abc" {
		t.Errorf("header value = %q, want 'Bearer sk_live_abc'", got)
	}
}

// TestApplySecretOverlaysBody overlays into a JSON body field.
func TestApplySecretOverlaysBody(t *testing.T) {
	refs := []secrets.Ref{{Path: "body.client_secret", Secret: "OAUTH_SECRET"}}
	var bag secrets.SecretBag
	bag.Set("OAUTH_SECRET", []byte("opaq_oauth_xyz"))
	headerOverlays := map[string]string{}
	body, err := applySecretOverlays(refs, bag, []byte(`{"client_id":"abc"}`), headerOverlays)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Contains(body, []byte(`"client_secret":"opaq_oauth_xyz"`)) {
		t.Errorf("body did not include client_secret: %s", body)
	}
	if !bytes.Contains(body, []byte(`"client_id":"abc"`)) {
		t.Errorf("body lost client_id: %s", body)
	}
	if len(headerOverlays) != 0 {
		t.Errorf("body-only ref should not write headers")
	}
}

// TestApplySecretOverlaysBodyOnEmpty seeds an empty body so sjson has
// somewhere to write.
func TestApplySecretOverlaysBodyOnEmpty(t *testing.T) {
	refs := []secrets.Ref{{Path: "body.api_key", Secret: "K"}}
	var bag secrets.SecretBag
	bag.Set("K", []byte("v"))
	body, err := applySecretOverlays(refs, bag, nil, map[string]string{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !bytes.Contains(body, []byte(`"api_key":"v"`)) {
		t.Errorf("body should contain api_key: %s", body)
	}
}

// TestApplySecretOverlaysMixed combines a formatted header and a body
// field — the case that motivates having both code paths.
func TestApplySecretOverlaysMixed(t *testing.T) {
	refs := []secrets.Ref{
		{Path: "headers.authorization", Secret: "STRIPE_KEY", Format: "Bearer {}"},
		{Path: "body.signature", Secret: "WEBHOOK_SIG"},
	}
	var bag secrets.SecretBag
	bag.Set("STRIPE_KEY", []byte("sk_live_abc"))
	bag.Set("WEBHOOK_SIG", []byte("hexdigest123"))
	headerOverlays := map[string]string{}
	body, err := applySecretOverlays(refs, bag, []byte(`{"event":"x"}`), headerOverlays)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if headerOverlays["authorization"] != "Bearer sk_live_abc" {
		t.Errorf("header wrong: %q", headerOverlays["authorization"])
	}
	if !bytes.Contains(body, []byte(`"signature":"hexdigest123"`)) {
		t.Errorf("body missing signature: %s", body)
	}
}

func TestApplySecretOverlaysMissingSecret(t *testing.T) {
	refs := []secrets.Ref{{Path: "headers.x", Secret: "NOT_IN_BAG"}}
	var bag secrets.SecretBag
	bag.Set("OTHER", []byte("v"))
	_, err := applySecretOverlays(refs, bag, []byte(`{}`), map[string]string{})
	if err == nil {
		t.Errorf("expected error for secret not in bag")
	}
	if !strings.Contains(err.Error(), "NOT_IN_BAG") {
		t.Errorf("error should mention the secret name, got: %v", err)
	}
}

func TestApplySecretOverlaysBadFormat(t *testing.T) {
	refs := []secrets.Ref{{Path: "headers.x", Secret: "K", Format: "no placeholder"}}
	var bag secrets.SecretBag
	bag.Set("K", []byte("v"))
	_, err := applySecretOverlays(refs, bag, []byte(`{}`), map[string]string{})
	if err == nil {
		t.Errorf("expected error for invalid format")
	}
}

func TestApplySecretOverlaysUnsupportedPath(t *testing.T) {
	refs := []secrets.Ref{{Path: "query.api_key", Secret: "K"}}
	var bag secrets.SecretBag
	bag.Set("K", []byte("v"))
	_, err := applySecretOverlays(refs, bag, []byte(`{}`), map[string]string{})
	if err == nil {
		t.Errorf("expected error for unsupported path namespace")
	}
	if !strings.Contains(err.Error(), "headers. or body.") {
		t.Errorf("error should explain supported paths, got: %v", err)
	}
}

// TestApplySecretOverlaysNoLeakInBody is the no-leak structural test
// for the overlay path. The cleartext appears in the modified body
// (it has to — that's where it's going), but op.Input bytes are not
// passed in by reference, so we verify the caller's input bytes are
// untouched.
func TestApplySecretOverlaysOriginalBodyUntouched(t *testing.T) {
	refs := []secrets.Ref{{Path: "body.client_secret", Secret: "K"}}
	var bag secrets.SecretBag
	bag.Set("K", []byte("sk_super_secret"))
	original := []byte(`{"client_id":"abc"}`)
	originalCopy := make([]byte, len(original))
	copy(originalCopy, original)
	_, err := applySecretOverlays(refs, bag, original, map[string]string{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// The original input slice MUST not have been mutated in place;
	// sjson.SetBytes may return the same slice if there's capacity,
	// but for our callsite (op.Input []byte) the contract is "don't
	// mutate". Verify the bytes we captured are still identical.
	if !bytes.Equal(original[:len(originalCopy)], originalCopy) {
		t.Errorf("original input bytes mutated; sjson.SetBytes is in-place")
	}
}
