package ops

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// BasicAuthVerify is the handler for `txco://basic-auth-verify`. It checks
// an inbound `Authorization: Basic <base64(user:password)>` header against
// a literal user and a password held in the per-op secret bag, and writes
// a verdict — never the credential — so a later rule can branch.
//
// It is the inbound counterpart to txco://basic-auth-encode and the
// same shape as txco://hmac-verify: a txcl `==` cannot see the secret
// bag and would short-circuit on the first differing byte, and encode's
// output (the base64 token) is itself the wire credential, which must
// not be written to the envelope for a comparison and then linger in
// every step trace. Here the presented user and password are compared
// in constant time, both halves always evaluated, and the decoded bytes
// are wiped.
//
// Gate it with a WHEN on the route; it runs only when its rule fires.
// A rule after it acts on the verdict:
//
//	WHEN @computed.basic_auth_ok != true
//	  EMIT @web.res.status = 401,
//	       @web.res.headers.www-authenticate.0 = "Basic realm=provision",
//	       @halt = true
//
// WITH parameters (op.Meta):
//
//	user                     = "provision"                          // required: the expected user-id
//	secrets.password.secret  = "PROVISION_PASSWORD"                 // required: NAME of the password secret
//	secrets.password.optional = true                                // the secret MAY be unset (see below)
//	allow_unconfigured       = true                                 // unset secret ⇒ ok=true (default false)
//	header_path              = "_txc.web.req.headers.Authorization.0" // gjson path to the header value
//	output_path              = "_txc.computed.basic_auth_ok"        // sjson path for the verdict (bool)
//	configured_path          = "_txc.computed.basic_auth_configured" // sjson path: was a password present (bool)
//
// A missing, empty, or malformed header, another scheme, or a wrong
// user or password is NOT an op error — it is ok=false (fail closed).
// Config problems (no user, no secret ref, no bag) are errors.
//
// "Unconfigured" is reachable only with `secrets.password.optional =
// true`: the processor otherwise refuses to run an op whose secret it
// cannot materialize. Then the rule decides what absence means through
// `allow_unconfigured` — true is the loopback-demo shape ("open until a
// password is set"); the default false keeps a route closed when the
// secret is missing OR misnamed. `basic_auth_configured` says which
// case produced the verdict, so a log or a trace can tell "let in" from
// "checked".
func BasicAuthVerify(ctx context.Context, opName string, in, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	bag := secrets.BagFromContext(ctx)

	secretRef := gjson.GetBytes(meta, "secrets.password.secret").String()
	user := gjson.GetBytes(meta, "user").String()
	allowUnconfigured := gjson.GetBytes(meta, "allow_unconfigured").Bool()
	headerPath := gjson.GetBytes(meta, "header_path").String()
	if headerPath == "" {
		headerPath = "_txc.web.req.headers.Authorization.0"
	}
	outputPath := gjson.GetBytes(meta, "output_path").String()
	if outputPath == "" {
		outputPath = "_txc.computed.basic_auth_ok"
	}
	configuredPath := gjson.GetBytes(meta, "configured_path").String()
	if configuredPath == "" {
		configuredPath = "_txc.computed.basic_auth_configured"
	}

	if secretRef == "" {
		return basicAuthVerifyErr("basic-auth-verify: missing secrets.password.secret in WITH"),
			errors.New("basic-auth-verify: missing secrets.password.secret")
	}
	if user == "" {
		return basicAuthVerifyErr("basic-auth-verify: missing user in WITH"),
			errors.New("basic-auth-verify: missing user")
	}
	if bag == nil {
		return basicAuthVerifyErr("basic-auth-verify: no secret bag on context"),
			errors.New("basic-auth-verify: bag missing")
	}

	password, configured := bag.Get(secretRef)
	ok := false
	if configured {
		ok = basicAuthMatches(gjson.GetBytes(in, headerPath).String(), user, password)
	} else {
		ok = allowUnconfigured
	}

	resp, err := sjson.Set(`{}`, outputPath, ok)
	if err == nil {
		resp, err = sjson.Set(resp, configuredPath, configured)
	}
	if err != nil {
		return basicAuthVerifyErr(fmt.Sprintf("basic-auth-verify: sjson set: %v", err)),
			fmt.Errorf("basic-auth-verify: sjson set: %w", err)
	}
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// basicAuthMatches parses `Basic <token>` (RFC 7617: scheme
// case-insensitive, token = base64(user-id ":" password), user-id may
// not contain a colon) and compares both halves in constant time. Both
// comparisons always run so timing does not reveal which half failed;
// length differences are the one thing ConstantTimeCompare reveals,
// the same trade-off hmac.Equal makes.
func basicAuthMatches(header, user string, password []byte) bool {
	fields := strings.Fields(header)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Basic") {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return false
	}
	defer secrets.Zero(raw)
	colon := bytes.IndexByte(raw, ':')
	if colon < 0 {
		return false
	}
	u := subtle.ConstantTimeCompare(raw[:colon], []byte(user))
	p := subtle.ConstantTimeCompare(raw[colon+1:], password)
	return u&p == 1
}

func basicAuthVerifyErr(msg string) event.Payload {
	em, _ := sjson.Set(`{}`, "error.0", "basic-auth-verify-err")
	em, _ = sjson.Set(em, "errorMsg", msg)
	return event.Payload{Raw: `{}`, Type: event.Null, Meta: em}
}
