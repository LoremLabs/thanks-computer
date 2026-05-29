// Package ops hosts chassis-core op handlers that consume the
// per-tenant secret store. These are computed-secret ops: they read
// cleartext from op.Secrets (via context), compute a digest or
// signature in-process, and write the **non-secret derived value**
// back into the response envelope. The cleartext is consumed once
// inside the op and goes nowhere else.
//
// See internal docs/todo-secret-store.md §4.2 for the substitution-vs-
// computation split.
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

// HMACSign is the handler for `txco://hmac-sign`. It HMACs an
// envelope-path value with a named secret key and writes the digest
// at a declared output path.
//
// WITH parameters (op.Meta):
//
//	secrets.key.secret  = "WEBHOOK_HMAC"   // required: NAME in op.Secrets
//	algorithm           = "sha256"         // sha256 (default) | sha512
//	input_path          = "body"           // gjson path on op.Input
//	output_path         = "_txc.computed.sig"  // sjson path on the response
//	encoding            = "hex"            // hex (default) | base64
//
// The handler reads `op.Secrets["WEBHOOK_HMAC"]` (the splice puts it
// there during processor.Run), computes
// `HMAC-<alg>(secret, gjson.Get(op.Input, input_path).Raw)`, and
// writes the digest into the response at output_path. The response
// is JSON; output_path defaults to `_txc.computed.hmac` if absent.
//
// Cleartext is consumed inside this function and never leaves it —
// the digest is the safe-to-publish artifact (one-way function of
// input+key).
func HMACSign(ctx context.Context, opName string, in, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	bag := secrets.BagFromContext(ctx)

	// Extract the parameters from op.Meta-on-ctx (set by the
	// processor's ExecCore wrapper alongside WithBag).
	secretRef := gjson.GetBytes(meta, "secrets.key.secret").String()
	algorithm := gjson.GetBytes(meta, "algorithm").String()
	if algorithm == "" {
		algorithm = "sha256"
	}
	inputPath := gjson.GetBytes(meta, "input_path").String()
	if inputPath == "" {
		inputPath = "body"
	}
	outputPath := gjson.GetBytes(meta, "output_path").String()
	if outputPath == "" {
		outputPath = "_txc.computed.hmac"
	}
	encoding := gjson.GetBytes(meta, "encoding").String()
	if encoding == "" {
		encoding = "hex"
	}

	if secretRef == "" {
		return errPayload("hmac-sign: missing secrets.key.secret in WITH"), errors.New("hmac-sign: missing secrets.key.secret")
	}
	if bag == nil {
		return errPayload("hmac-sign: no secret bag on context"), errors.New("hmac-sign: bag missing")
	}
	key, ok := bag.Get(secretRef)
	if !ok {
		return errPayload(fmt.Sprintf("hmac-sign: secret %q not materialized", secretRef)),
			fmt.Errorf("hmac-sign: secret %q not in op.Secrets", secretRef)
	}

	var hashFn func() hash.Hash
	switch algorithm {
	case "sha256":
		hashFn = sha256.New
	case "sha512":
		hashFn = sha512.New
	default:
		return errPayload(fmt.Sprintf("hmac-sign: unsupported algorithm %q (sha256|sha512)", algorithm)),
			fmt.Errorf("hmac-sign: unsupported algorithm %q", algorithm)
	}

	// Extract the input bytes to sign. `.Raw` preserves whitespace
	// and field ordering (load-bearing for HMACs of JSON bodies —
	// vendors compute over the byte sequence they received).
	inputBytes := []byte(gjson.GetBytes(in, inputPath).Raw)
	if len(inputBytes) == 0 {
		// Treat "missing path" as "sign the empty string" so an
		// operator can opt-in by setting input_path="" or pointing
		// at a key that doesn't exist (matches what most webhook
		// schemes do for empty bodies).
	}

	mac := hmac.New(hashFn, key)
	mac.Write(inputBytes)
	sum := mac.Sum(nil)

	var encoded string
	switch encoding {
	case "hex":
		encoded = hex.EncodeToString(sum)
	case "base64":
		encoded = base64.StdEncoding.EncodeToString(sum)
	default:
		return errPayload(fmt.Sprintf("hmac-sign: unsupported encoding %q (hex|base64)", encoding)),
			fmt.Errorf("hmac-sign: unsupported encoding %q", encoding)
	}

	// Build response envelope: write the (non-secret) digest at
	// output_path. The chassis merge folds it into the per-scope
	// merged response, so downstream ops can reference it via
	// normal envelope SET / gjson reads.
	resp := `{}`
	resp, err := sjson.Set(resp, outputPath, encoded)
	if err != nil {
		return errPayload(fmt.Sprintf("hmac-sign: sjson set %q: %v", outputPath, err)),
			fmt.Errorf("hmac-sign: sjson set: %w", err)
	}
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// errPayload builds a structured error event.Payload. NEVER includes
// any secret cleartext — only the human-readable reason for the
// failure.
func errPayload(msg string) event.Payload {
	em, _ := sjson.Set(`{}`, "error.0", "hmac-sign-err")
	em, _ = sjson.Set(em, "errorMsg", msg)
	return event.Payload{Raw: `{}`, Type: event.Null, Meta: em}
}
