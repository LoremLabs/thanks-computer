package ops

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// BasicAuthEncode is the handler for `txco://basic-auth-encode`. It
// produces the `base64(user:password)` value used by HTTP Basic auth
// (RFC 7617), with the password coming from op.Secrets.
//
// WITH parameters (op.Meta):
//
//	secrets.password.secret = "TWILIO_AUTH_TOKEN"  // required
//	user                    = "AC1234567890"        // required
//	output_path             = "_txc.computed.basic_auth"  // default
//
// The handler writes the encoded value (NOT the cleartext) at
// output_path. Downstream ops set the Authorization header as
// `Basic <encoded>` — typically via a `secrets.headers.authorization`
// declaration with `format = "Basic {output_path-value}"`, but since
// the encoded value is not itself a secret, simpler: just a normal
// SET that interpolates the envelope value into the header.
//
// Cleartext is consumed inside this function and never leaves it.
// The base64 encoding is one-way only in the sense that base64-
// decoding reveals the cleartext — DO NOT trace this output as
// "non-secret"; it IS a wire credential. The op writes it at
// _txc.computed.* by default, where the operator chooses how to
// route it.
func BasicAuthEncode(ctx context.Context, opName string, in, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	bag := secrets.BagFromContext(ctx)

	secretRef := gjson.GetBytes(meta, "secrets.password.secret").String()
	user := gjson.GetBytes(meta, "user").String()
	outputPath := gjson.GetBytes(meta, "output_path").String()
	if outputPath == "" {
		outputPath = "_txc.computed.basic_auth"
	}

	if secretRef == "" {
		return basicAuthErr("basic-auth-encode: missing secrets.password.secret in WITH"),
			errors.New("basic-auth-encode: missing secrets.password.secret")
	}
	if user == "" {
		return basicAuthErr("basic-auth-encode: missing user in WITH"),
			errors.New("basic-auth-encode: missing user")
	}
	if bag == nil {
		return basicAuthErr("basic-auth-encode: no secret bag on context"),
			errors.New("basic-auth-encode: bag missing")
	}
	password, ok := bag.Get(secretRef)
	if !ok {
		return basicAuthErr(fmt.Sprintf("basic-auth-encode: secret %q not materialized", secretRef)),
			fmt.Errorf("basic-auth-encode: secret %q not in op.Secrets", secretRef)
	}

	// RFC 7617: token = base64(user-id ":" password). Build into a
	// fresh byte slice we control. The intermediate `joined` holds
	// cleartext briefly; it's released to GC after EncodeToString.
	joined := make([]byte, 0, len(user)+1+len(password))
	joined = append(joined, user...)
	joined = append(joined, ':')
	joined = append(joined, password...)
	encoded := base64.StdEncoding.EncodeToString(joined)
	// Wipe the intermediate; the encoded value still contains the
	// cleartext in base64, but it's the operator's choice where it
	// goes from here (typically the Authorization header).
	secrets.Zero(joined)

	resp := `{}`
	resp, err := sjson.Set(resp, outputPath, encoded)
	if err != nil {
		return basicAuthErr(fmt.Sprintf("basic-auth-encode: sjson set %q: %v", outputPath, err)),
			fmt.Errorf("basic-auth-encode: sjson set: %w", err)
	}
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

func basicAuthErr(msg string) event.Payload {
	em, _ := sjson.Set(`{}`, "error.0", "basic-auth-encode-err")
	em, _ = sjson.Set(em, "errorMsg", msg)
	return event.Payload{Raw: `{}`, Type: event.Null, Meta: em}
}
