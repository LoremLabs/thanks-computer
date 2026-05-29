package ops

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
)

func withMeta(meta string) context.Context {
	return operation.WithMeta(context.Background(), meta)
}

func TestCopyStringRaw(t *testing.T) {
	in := []byte(`{"text":"hello world"}`)
	ctx := withMeta(`{"from":".text","to":"._txc.web.res.body"}`)

	out, err := Copy(ctx, "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got := gjson.Get(out.Raw, "_txc.web.res.body").String()
	if got != "hello world" {
		t.Errorf("copied value = %q, want %q", got, "hello world")
	}
}

func TestCopyStringBase64(t *testing.T) {
	in := []byte(`{"text":"hello world"}`)
	ctx := withMeta(`{"from":".text","to":"._txc.web.res.body","encode":"base64"}`)

	out, err := Copy(ctx, "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got := gjson.Get(out.Raw, "_txc.web.res.body").String()
	decoded, derr := base64.StdEncoding.DecodeString(got)
	if derr != nil {
		t.Fatalf("base64 decode: %v (raw=%q)", derr, got)
	}
	if string(decoded) != "hello world" {
		t.Errorf("decoded = %q, want %q", decoded, "hello world")
	}
}

func TestCopyStructuredRaw(t *testing.T) {
	// Copying a non-string value (object, array, number) preserves
	// shape when encode == "" — uses sjson.SetRaw under the hood.
	in := []byte(`{"data":{"x":1,"y":[1,2]}}`)
	ctx := withMeta(`{"from":".data","to":".cloned"}`)

	out, err := Copy(ctx, "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	clonedX := gjson.Get(out.Raw, "cloned.x").Int()
	if clonedX != 1 {
		t.Errorf("cloned.x = %d, want 1", clonedX)
	}
	clonedY := gjson.Get(out.Raw, "cloned.y").Array()
	if len(clonedY) != 2 || clonedY[0].Int() != 1 || clonedY[1].Int() != 2 {
		t.Errorf("cloned.y = %v, want [1,2]", clonedY)
	}
}

func TestCopyMissingSourceIsEmpty(t *testing.T) {
	// A missing source path is not an error — copies "" — so an
	// optional "if present, copy it" idiom can layer with `WHEN` on
	// the caller's side without making Copy itself defensive.
	in := []byte(`{}`)
	ctx := withMeta(`{"from":".text","to":".out"}`)

	out, err := Copy(ctx, "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := gjson.Get(out.Raw, "out").String(); got != "" {
		t.Errorf("missing-source out = %q, want \"\"", got)
	}
}

func TestCopyMissingFromFailsLoud(t *testing.T) {
	in := []byte(`{"text":"hi"}`)
	ctx := withMeta(`{"to":".out"}`)

	_, err := Copy(ctx, "txco://copy", in, nil)
	if err == nil {
		t.Fatal("Copy with missing `from` returned nil error")
	}
	if !strings.Contains(err.Error(), "from") {
		t.Errorf("err = %v, want one mentioning `from`", err)
	}
}

func TestCopyMissingToFailsLoud(t *testing.T) {
	in := []byte(`{"text":"hi"}`)
	ctx := withMeta(`{"from":".text"}`)

	_, err := Copy(ctx, "txco://copy", in, nil)
	if err == nil {
		t.Fatal("Copy with missing `to` returned nil error")
	}
	if !strings.Contains(err.Error(), "to") {
		t.Errorf("err = %v, want one mentioning `to`", err)
	}
}

func TestCopyRejectsUnknownEncode(t *testing.T) {
	in := []byte(`{"text":"hi"}`)
	ctx := withMeta(`{"from":".text","to":".out","encode":"rot13"}`)

	_, err := Copy(ctx, "txco://copy", in, nil)
	if err == nil {
		t.Fatal("Copy with unknown encode returned nil error")
	}
	if !strings.Contains(err.Error(), "rot13") {
		t.Errorf("err = %v, want one naming the bad encode", err)
	}
}

func TestCopyAcceptsBareOrDottedPath(t *testing.T) {
	// Path strings can come in with or without the leading dot —
	// both should work identically.
	in := []byte(`{"text":"hello"}`)
	for _, m := range []string{
		`{"from":".text","to":".out"}`,
		`{"from":"text","to":"out"}`,
	} {
		out, err := Copy(withMeta(m), "txco://copy", in, nil)
		if err != nil {
			t.Fatalf("Copy(%s): %v", m, err)
		}
		if got := gjson.Get(out.Raw, "out").String(); got != "hello" {
			t.Errorf("meta=%s out=%q, want hello", m, got)
		}
	}
}

func TestCopyAcceptsAtSugar(t *testing.T) {
	// `@` is sugar for `_txc.` — same convention WHEN uses. Rule
	// authors can write `@web.req.url.query.repoName.0` in WITH
	// params and have it resolve to the chassis-internal path.
	in := []byte(`{"_txc":{"web":{"req":{"url":{"query":{"repoName":["facebook/react"]}}}}}}`)
	out, err := Copy(withMeta(`{"from":"@web.req.url.query.repoName.0","to":".repoName"}`), "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := gjson.Get(out.Raw, "repoName").String(); got != "facebook/react" {
		t.Errorf("@-path copy = %q, want facebook/react", got)
	}
}

func TestCopyAtSugarWorksOnBothSides(t *testing.T) {
	// Both `from` and `to` should accept @ sugar.
	in := []byte(`{"_txc":{"src":"http"}}`)
	out, err := Copy(withMeta(`{"from":"@src","to":"@computed.src"}`), "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := gjson.Get(out.Raw, "_txc.computed.src").String(); got != "http" {
		t.Errorf("@→@ copy = %q, want http (raw=%s)", got, out.Raw)
	}
}

func TestCopyDefaultUsedWhenSourceEmpty(t *testing.T) {
	// `default` is the literal substituted when `from` resolves
	// empty/missing — the "query-param with default" idiom.
	in := []byte(`{}`)
	out, err := Copy(withMeta(`{"from":"@web.req.url.query.repoName.0","to":".repoName","default":"loremlabs/kudos"}`), "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := gjson.Get(out.Raw, "repoName").String(); got != "loremlabs/kudos" {
		t.Errorf("default fallback = %q, want loremlabs/kudos", got)
	}
}

func TestCopyDefaultSkippedWhenSourcePresent(t *testing.T) {
	// `default` MUST NOT clobber a present value.
	in := []byte(`{"_txc":{"web":{"req":{"url":{"query":{"repoName":["facebook/react"]}}}}}}`)
	out, err := Copy(withMeta(`{"from":"@web.req.url.query.repoName.0","to":".repoName","default":"loremlabs/kudos"}`), "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if got := gjson.Get(out.Raw, "repoName").String(); got != "facebook/react" {
		t.Errorf("present source overridden by default: got %q", got)
	}
}

func TestCopyDefaultRespectsEncode(t *testing.T) {
	// When `default` is substituted, encoding still applies — so
	// `encode=base64` works for both real and defaulted sources.
	in := []byte(`{}`)
	out, err := Copy(withMeta(`{"from":".x","to":".y","default":"hello","encode":"base64"}`), "txco://copy", in, nil)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	b64 := gjson.Get(out.Raw, "y").String()
	decoded, _ := base64.StdEncoding.DecodeString(b64)
	if string(decoded) != "hello" {
		t.Errorf("default + base64 = %q, want hello", decoded)
	}
}
