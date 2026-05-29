package secrets

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRefsEmptyMeta(t *testing.T) {
	for _, in := range []string{``, `{}`, `{"timeout":1000}`} {
		refs, err := ParseRefs(in)
		if err != nil {
			t.Errorf("meta=%q: unexpected error: %v", in, err)
		}
		if refs != nil {
			t.Errorf("meta=%q: expected nil refs, got %+v", in, refs)
		}
	}
}

func TestParseRefsSingleHeader(t *testing.T) {
	meta := `{"secrets":{"headers":{"authorization":{"secret":"STRIPE_API_KEY","format":"Bearer {}"}}}}`
	refs, err := ParseRefs(meta)
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}
	want := []Ref{{Path: "headers.authorization", Secret: "STRIPE_API_KEY", Format: "Bearer {}"}}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("got %+v, want %+v", refs, want)
	}
}

func TestParseRefsRawHeaderNoFormat(t *testing.T) {
	meta := `{"secrets":{"headers":{"x-api-key":{"secret":"VENDOR_KEY"}}}}`
	refs, err := ParseRefs(meta)
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}
	want := []Ref{{Path: "headers.x-api-key", Secret: "VENDOR_KEY", Format: ""}}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("got %+v, want %+v", refs, want)
	}
}

func TestParseRefsBodyPath(t *testing.T) {
	meta := `{"secrets":{"body":{"client_secret":{"secret":"OAUTH_SECRET"}}}}`
	refs, err := ParseRefs(meta)
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}
	if len(refs) != 1 || refs[0].Path != "body.client_secret" || refs[0].Secret != "OAUTH_SECRET" {
		t.Errorf("got %+v", refs)
	}
}

func TestParseRefsMultipleScattered(t *testing.T) {
	meta := `{"secrets":{
		"headers":{
			"authorization":{"secret":"STRIPE_API_KEY","format":"Bearer {}"},
			"x-signature":{"secret":"WEBHOOK_HMAC"}
		},
		"body":{
			"client_secret":{"secret":"OAUTH_SECRET"}
		}
	}}`
	refs, err := ParseRefs(meta)
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}
	if len(refs) != 3 {
		t.Fatalf("len(refs) = %d, want 3, got %+v", len(refs), refs)
	}
	// Find each by path.
	byPath := map[string]Ref{}
	for _, r := range refs {
		byPath[r.Path] = r
	}
	if r := byPath["headers.authorization"]; r.Secret != "STRIPE_API_KEY" || r.Format != "Bearer {}" {
		t.Errorf("headers.authorization wrong: %+v", r)
	}
	if r := byPath["headers.x-signature"]; r.Secret != "WEBHOOK_HMAC" || r.Format != "" {
		t.Errorf("headers.x-signature wrong: %+v", r)
	}
	if r := byPath["body.client_secret"]; r.Secret != "OAUTH_SECRET" {
		t.Errorf("body.client_secret wrong: %+v", r)
	}
}

func TestParseRefsRejectsBareValue(t *testing.T) {
	// Operator wrote `secrets.headers.x-api-key = "VENDOR_KEY"` (bare
	// string) instead of `secrets.headers.x-api-key.secret = ...`.
	meta := `{"secrets":{"headers":{"x-api-key":"VENDOR_KEY"}}}`
	_, err := ParseRefs(meta)
	if err == nil {
		t.Fatalf("expected error for bare-value leaf")
	}
	if !strings.Contains(err.Error(), "bare value") {
		t.Errorf("expected 'bare value' in error, got: %v", err)
	}
}

func TestParseRefsRejectsTopLevelLeafKeys(t *testing.T) {
	// `secrets.secret = "X"` at the top level — no path, malformed.
	meta := `{"secrets":{"secret":"STRIPE"}}`
	_, err := ParseRefs(meta)
	if err == nil {
		t.Fatalf("expected error for top-level 'secret' key")
	}
}

func TestParseRefsRejectsEmptySecretName(t *testing.T) {
	meta := `{"secrets":{"headers":{"x":{"secret":""}}}}`
	_, err := ParseRefs(meta)
	if err == nil {
		t.Fatalf("expected error for empty .secret value")
	}
}

func TestParseRefsRejectsBadFormat(t *testing.T) {
	// .format without `{}` is invalid per ValidateFormat.
	meta := `{"secrets":{"headers":{"x":{"secret":"K","format":"no placeholder"}}}}`
	_, err := ParseRefs(meta)
	if err == nil {
		t.Fatalf("expected error for format missing placeholder")
	}
}

func TestParseRefsRejectsNonStringSecret(t *testing.T) {
	meta := `{"secrets":{"headers":{"x":{"secret":123}}}}`
	_, err := ParseRefs(meta)
	if err == nil {
		t.Fatalf("expected error for non-string .secret")
	}
}

func TestParseRefsRejectsNonStringFormat(t *testing.T) {
	meta := `{"secrets":{"headers":{"x":{"secret":"K","format":42}}}}`
	_, err := ParseRefs(meta)
	if err == nil {
		t.Fatalf("expected error for non-string .format")
	}
}

func TestParseRefsRejectsNonObjectSecretsRoot(t *testing.T) {
	meta := `{"secrets":"oops"}`
	_, err := ParseRefs(meta)
	if err == nil {
		t.Fatalf("expected error for non-object secrets")
	}
}

func TestDistinctNames(t *testing.T) {
	refs := []Ref{
		{Path: "headers.a", Secret: "FOO"},
		{Path: "body.b", Secret: "FOO"}, // same name, different path
		{Path: "headers.c", Secret: "BAR"},
	}
	got := DistinctNames(refs)
	want := []string{"FOO", "BAR"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DistinctNames: got %v, want %v", got, want)
	}
	if d := DistinctNames(nil); d != nil {
		t.Errorf("DistinctNames(nil) = %v, want nil", d)
	}
}

func TestHasRefs(t *testing.T) {
	if HasRefs("") {
		t.Errorf("HasRefs(\"\") should be false")
	}
	if HasRefs(`{}`) {
		t.Errorf("HasRefs(empty obj) should be false")
	}
	if HasRefs(`{"timeout":1000}`) {
		t.Errorf("HasRefs(meta with no 'secrets' key) should be false")
	}
	if !HasRefs(`{"secrets":{"x":{"secret":"K"}}}`) {
		t.Errorf("HasRefs(meta with 'secrets') should be true")
	}
}
