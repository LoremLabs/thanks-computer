package ops

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// HMACVerify is the handler for `txco://hmac-verify`. It recomputes an
// HMAC over an envelope-path value with a named secret key and
// constant-time-compares it to an expected signature carried on the
// envelope, writing a boolean result at output_path.
//
// It is the verification counterpart to txco://hmac-sign, and it lives
// as an op (not a txcl function) for two reasons functions can't
// satisfy: the keyed HMAC needs the per-op secret bag (txcl functions
// never see it), and the comparison must be constant-time (a txcl `==`
// short-circuits and leaks timing). This is the primitive behind
// signed-webhook verification — Stripe / GitHub / Shopify / Slack all
// use minor variants of HMAC-over-body.
//
// Gate it with a WHEN like any op; it only runs when its rule fires.
// It adds `sig_valid` to the response so a later rule can branch, e.g.:
//
//	WHEN ._txc.computed.sig_valid != true
//	  EXEC "txco://..."   # reject / 401
//
// WITH parameters (op.Meta):
//
//	secrets.key.secret = "STRIPE_WEBHOOK_SECRET"   // required: NAME in op.Secrets
//	algorithm          = "sha256"                  // sha256 (default) | sha512
//	input_path         = "verify.signed"           // gjson path to the signed data
//	expected_path      = "verify.v1"               // required: gjson path to the expected sig
//	encoding           = "hex"                     // hex (default) | base64 (of the expected sig)
//	output_path        = "_txc.computed.sig_valid" // sjson path for the bool result
//
// String inputs are hashed as their literal (unquoted) value, so a
// scheme that signs a constructed string like Stripe's "t.body" works;
// object/array inputs use the raw JSON bytes (preserving whitespace and
// key order, which is what vendors sign).
//
// Config problems (missing secret ref or expected_path, unmaterialized
// secret, bad algorithm/encoding) return an error. A missing, empty, or
// malformed expected signature is NOT an error — it's a failed
// verification, so the op emits sig_valid=false (fail closed).
//
// The op emits ONLY the boolean — never the recomputed digest, which is
// itself a valid signature and must not leak into the envelope/trace.
func HMACVerify(ctx context.Context, opName string, in, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	bag := secrets.BagFromContext(ctx)

	secretRef := gjson.GetBytes(meta, "secrets.key.secret").String()
	algorithm := gjson.GetBytes(meta, "algorithm").String()
	if algorithm == "" {
		algorithm = "sha256"
	}
	inputPath := gjson.GetBytes(meta, "input_path").String()
	if inputPath == "" {
		inputPath = "body"
	}
	expectedPath := gjson.GetBytes(meta, "expected_path").String()
	outputPath := gjson.GetBytes(meta, "output_path").String()
	if outputPath == "" {
		outputPath = "_txc.computed.sig_valid"
	}
	encoding := gjson.GetBytes(meta, "encoding").String()
	if encoding == "" {
		encoding = "hex"
	}

	if secretRef == "" {
		return verifyErrPayload("hmac-verify: missing secrets.key.secret in WITH"), errors.New("hmac-verify: missing secrets.key.secret")
	}
	if expectedPath == "" {
		return verifyErrPayload("hmac-verify: missing expected_path in WITH"), errors.New("hmac-verify: missing expected_path")
	}
	if bag == nil {
		return verifyErrPayload("hmac-verify: no secret bag on context"), errors.New("hmac-verify: bag missing")
	}
	key, ok := bag.Get(secretRef)
	if !ok {
		return verifyErrPayload(fmt.Sprintf("hmac-verify: secret %q not materialized", secretRef)),
			fmt.Errorf("hmac-verify: secret %q not in op.Secrets", secretRef)
	}

	var hashFn func() hash.Hash
	switch algorithm {
	case "sha256":
		hashFn = sha256.New
	case "sha512":
		hashFn = sha512.New
	default:
		return verifyErrPayload(fmt.Sprintf("hmac-verify: unsupported algorithm %q (sha256|sha512)", algorithm)),
			fmt.Errorf("hmac-verify: unsupported algorithm %q", algorithm)
	}

	// Input bytes — IDENTICAL handling to hmac-sign (shared hmacInputBytes), so
	// a signature produced there verifies here.
	inputBytes := hmacInputBytes(in, inputPath)

	mac := hmac.New(hashFn, key)
	mac.Write(inputBytes)
	sum := mac.Sum(nil)

	// Decode the expected signature to raw bytes and compare in
	// constant time. Decode failure or length mismatch ⇒ invalid
	// (fail closed), not an op error.
	expected := gjson.GetBytes(in, expectedPath).String()
	var expectedBytes []byte
	var decErr error
	switch encoding {
	case "hex":
		expectedBytes, decErr = hex.DecodeString(expected)
	case "base64":
		expectedBytes, decErr = base64.StdEncoding.DecodeString(expected)
	default:
		return verifyErrPayload(fmt.Sprintf("hmac-verify: unsupported encoding %q (hex|base64)", encoding)),
			fmt.Errorf("hmac-verify: unsupported encoding %q", encoding)
	}

	valid := decErr == nil && hmac.Equal(sum, expectedBytes)

	resp, err := sjson.Set(`{}`, outputPath, valid)
	if err != nil {
		return verifyErrPayload(fmt.Sprintf("hmac-verify: sjson set %q: %v", outputPath, err)),
			fmt.Errorf("hmac-verify: sjson set: %w", err)
	}
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// verifyErrPayload builds a structured error event.Payload for
// hmac-verify. NEVER includes secret cleartext or the recomputed
// digest — only the human-readable reason.
func verifyErrPayload(msg string) event.Payload {
	em, _ := sjson.Set(`{}`, "error.0", "hmac-verify-err")
	em, _ = sjson.Set(em, "errorMsg", msg)
	return event.Payload{Raw: `{}`, Type: event.Null, Meta: em}
}
