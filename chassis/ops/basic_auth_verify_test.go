package ops

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

const bavMeta = `{"user":"provision","secrets":{"password":{"secret":"PW","optional":true}}}`

func bavInput(t *testing.T, header string) []byte {
	t.Helper()
	in := `{}`
	if header != "" {
		in, _ = sjson.Set(in, "_txc.web.req.headers.Authorization.0", header)
	}
	return []byte(in)
}

func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func bavRun(t *testing.T, ctx context.Context, header string) (ok, configured bool, raw string) {
	t.Helper()
	out, err := BasicAuthVerify(ctx, "txco://basic-auth-verify", bavInput(t, header), nil)
	if err != nil {
		t.Fatalf("BasicAuthVerify: %v", err)
	}
	return gjson.Get(out.Raw, "_txc.computed.basic_auth_ok").Bool(),
		gjson.Get(out.Raw, "_txc.computed.basic_auth_configured").Bool(), out.Raw
}

func TestBasicAuthVerifyMatches(t *testing.T) {
	ctx := withBagAndMeta(t, "PW", []byte("s3cret"), bavMeta)
	ok, configured, raw := bavRun(t, ctx, basicHeader("provision", "s3cret"))
	if !ok || !configured {
		t.Errorf("ok=%v configured=%v, want true/true; resp=%s", ok, configured, raw)
	}
	// Scheme is case-insensitive.
	if ok, _, _ := bavRun(t, ctx, "basic "+base64.StdEncoding.EncodeToString([]byte("provision:s3cret"))); !ok {
		t.Errorf("lowercase scheme rejected")
	}
	// A password containing a colon splits at the FIRST colon only.
	ctx2 := withBagAndMeta(t, "PW", []byte("a:b:c"), bavMeta)
	if ok, _, _ := bavRun(t, ctx2, basicHeader("provision", "a:b:c")); !ok {
		t.Errorf("password with colons rejected")
	}
}

func TestBasicAuthVerifyRejects(t *testing.T) {
	ctx := withBagAndMeta(t, "PW", []byte("s3cret"), bavMeta)
	cases := map[string]string{
		"wrong password": basicHeader("provision", "s3cre"),
		"wrong user":     basicHeader("provisio", "s3cret"),
		"empty password": basicHeader("provision", ""),
		"missing header": "",
		"bearer scheme":  "Bearer " + base64.StdEncoding.EncodeToString([]byte("provision:s3cret")),
		"bad base64":     "Basic ***",
		"no colon":       "Basic " + base64.StdEncoding.EncodeToString([]byte("provisions3cret")),
		"extra token":    basicHeader("provision", "s3cret") + " extra",
	}
	for name, h := range cases {
		ok, configured, raw := bavRun(t, ctx, h)
		if ok || !configured {
			t.Errorf("%s: ok=%v configured=%v, want false/true; resp=%s", name, ok, configured, raw)
		}
	}
	// The verdict is all that is emitted — no credential material.
	_, _, raw := bavRun(t, ctx, basicHeader("provision", "s3cret"))
	if gjson.Get(raw, "_txc.computed.basic_auth").Exists() || len(raw) > 120 {
		t.Errorf("unexpected output shape: %s", raw)
	}
}

func TestBasicAuthVerifyUnconfigured(t *testing.T) {
	// The bag lacks PW (reachable in prod only via secrets.password.optional).
	var bag secrets.SecretBag
	bag.Set("OTHER", []byte("x"))
	base := secrets.WithBag(context.Background(), &bag)

	// Default: fail closed even with a "correct" header.
	ctx := operation.WithMeta(base, bavMeta)
	ok, configured, raw := bavRun(t, ctx, basicHeader("provision", "anything"))
	if ok || configured {
		t.Errorf("unconfigured default: ok=%v configured=%v, want false/false; resp=%s", ok, configured, raw)
	}
	// allow_unconfigured=true: the rule chose "unset ⇒ open"; header irrelevant.
	openMeta, _ := sjson.Set(bavMeta, "allow_unconfigured", true)
	ctx = operation.WithMeta(base, openMeta)
	for _, h := range []string{"", basicHeader("provision", "anything")} {
		ok, configured, raw = bavRun(t, ctx, h)
		if !ok || configured {
			t.Errorf("allow_unconfigured: ok=%v configured=%v, want true/false; resp=%s", ok, configured, raw)
		}
	}
}

func TestBasicAuthVerifyConfigErrors(t *testing.T) {
	ctx := withBagAndMeta(t, "PW", []byte("s3cret"), `{"secrets":{"password":{"secret":"PW"}}}`)
	if _, err := BasicAuthVerify(ctx, "txco://basic-auth-verify", bavInput(t, ""), nil); err == nil {
		t.Errorf("missing user accepted")
	}
	ctx = withBagAndMeta(t, "PW", []byte("s3cret"), `{"user":"provision"}`)
	if _, err := BasicAuthVerify(ctx, "txco://basic-auth-verify", bavInput(t, ""), nil); err == nil {
		t.Errorf("missing secret ref accepted")
	}
	ctx = operation.WithMeta(context.Background(), bavMeta)
	if _, err := BasicAuthVerify(ctx, "txco://basic-auth-verify", bavInput(t, ""), nil); err == nil {
		t.Errorf("missing bag accepted")
	}
}
