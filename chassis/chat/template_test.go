package chat

import (
	"errors"
	"strings"
	"testing"
)

func TestTemplateRendererEscapesString(t *testing.T) {
	// @path addresses _txc.* per the txcl convention (parser.go:216,234).
	envelope := []byte(`{"_txc":{"x":"line1\nline2 with \"quotes\" and \\back"}}`)
	got, err := Render(`prompt: {{@x}}`, envelope, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The substituted value must be JSON-escaped so it splices safely.
	// gjson decodes the envelope's escapes (\n -> newline, \" -> "),
	// then we re-encode them (\n, \", \\) without the outer quotes.
	want := `prompt: line1\nline2 with \"quotes\" and \\back`
	if got != want {
		t.Errorf("escape mismatch:\n got %q\nwant %q", got, want)
	}
}

func TestTemplateRendererRejectsBangForm(t *testing.T) {
	_, err := Render(`x={{!@field}}`, []byte(`{"field":"value"}`), nil)
	if err == nil {
		t.Fatalf("expected error for {{!@...}}; got nil")
	}
	var iw *InvalidWithError
	if !errors.As(err, &iw) {
		t.Fatalf("expected *InvalidWithError, got %T: %v", err, err)
	}
	if !strings.Contains(iw.Reason, "verbatim") {
		t.Errorf("error should mention verbatim form; got %q", iw.Reason)
	}
}

func TestTemplateRendererRejectsUnrecognizedMarker(t *testing.T) {
	_, err := Render(`x={{field}}`, []byte(`{"field":"value"}`), nil)
	if err == nil {
		t.Fatalf("expected error for {{field}} without @; got nil")
	}
	var iw *InvalidWithError
	if !errors.As(err, &iw) {
		t.Fatalf("expected *InvalidWithError, got %T: %v", err, err)
	}
}

func TestTemplateRendererJSONEncodesNonString(t *testing.T) {
	envelope := []byte(`{"_txc":{"obj":{"a":1,"b":[2,3]},"n":42,"b":true,"nul":null}}`)
	cases := []struct {
		template string
		want     string
	}{
		{`x={{@obj}}`, `x={"a":1,"b":[2,3]}`},
		{`x={{@n}}`, `x=42`},
		{`x={{@b}}`, `x=true`},
		{`x={{@nul}}`, `x=null`},
	}
	for _, c := range cases {
		got, err := Render(c.template, envelope, nil)
		if err != nil {
			t.Errorf("Render(%q): %v", c.template, err)
			continue
		}
		if got != c.want {
			t.Errorf("template %q:\n got %q\nwant %q", c.template, got, c.want)
		}
	}
}

func TestTemplateRendererMissingPathRendersEmpty(t *testing.T) {
	got, err := Render(`x={{@no.such.path}}.`, []byte(`{}`), nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "x=." {
		t.Errorf("missing path should substitute empty; got %q", got)
	}
}

// Regression: caught during ai://chat smoke 2026-06-01. The template
// renderer must mirror the txcl @path → _txc.path rewrite (parser.go:
// 216,234) so authors who write WHEN/SET/EMIT with @path get the same
// behavior in prompts. Before the fix, {{@body_text}} looked up
// "body_text" at the envelope root, returning empty since the field
// actually lives at "_txc.body_text" — and the LLM saw a blank prompt.
func TestTemplateRendererMatchesTxclAtConvention(t *testing.T) {
	// Authors write @body_text; the envelope holds _txc.body_text.
	envelope := []byte(`{"_txc":{"body_text":"tell me about hummingbirds","web":{"req":{"body":"dGVsbCBtZQ=="}}},"body_text":"this top-level value must NOT be used"}`)

	got, err := Render(`{{@body_text}}`, envelope, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "tell me about hummingbirds" {
		t.Errorf("@body_text resolved to %q; expected _txc.body_text", got)
	}

	got2, err := Render(`{{@web.req.body}}`, envelope, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got2 != "dGVsbCBtZQ==" {
		t.Errorf("@web.req.body resolved to %q; expected _txc.web.req.body", got2)
	}
}

func TestTemplateRendererNoMarkersIsPassthrough(t *testing.T) {
	in := "no markers here"
	got, err := Render(in, []byte(`{"x":"y"}`), nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestTemplateRendererMultipleSubstitutions(t *testing.T) {
	envelope := []byte(`{"_txc":{"a":"AAA","b":"BBB"}}`)
	got, err := Render(`{{@a}}-{{@b}}-{{@a}}`, envelope, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "AAA-BBB-AAA" {
		t.Errorf("got %q, want %q", got, "AAA-BBB-AAA")
	}
}

func TestTemplateRendererEmptyPathErrors(t *testing.T) {
	_, err := Render(`x={{@}}`, []byte(`{}`), nil)
	if err == nil {
		t.Fatalf("expected error for {{@}}; got nil")
	}
}

func TestTemplateRendererUnclosedMarkerIsPassthrough(t *testing.T) {
	// An unclosed `{{` should not error or hang — emit what the author
	// wrote so they can spot the typo.
	got, err := Render(`x={{@field`, []byte(`{"field":"v"}`), nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != `x={{@field` {
		t.Errorf("unclosed marker should be verbatim; got %q", got)
	}
}
