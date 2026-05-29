package oprefs

import (
	"strings"
	"testing"
)

func TestResolveOpRefsBasic(t *testing.T) {
	ops := map[string]Operation{
		"CLASSIFY": {URL: "http://localhost:4101/op"},
	}
	got, err := ResolveOpRefs(`EXEC "op://CLASSIFY"`, ops)
	if err != nil {
		t.Fatalf("ResolveOpRefs: %v", err)
	}
	want := `EXEC "http://localhost:4101/op"`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveOpRefsMultiple(t *testing.T) {
	ops := map[string]Operation{
		"CLASSIFY": {URL: "http://classify"},
		"NOTIFY":   {URL: "http://notify"},
	}
	in := `WHEN .x == 1 EXEC "op://CLASSIFY"
SET .next = "op://NOTIFY"`
	got, err := ResolveOpRefs(in, ops)
	if err != nil {
		t.Fatalf("ResolveOpRefs: %v", err)
	}
	want := `WHEN .x == 1 EXEC "http://classify"
SET .next = "http://notify"`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestResolveOpRefsMissingNameErrors(t *testing.T) {
	ops := map[string]Operation{
		"CLASSIFY": {URL: "http://classify"},
	}
	_, err := ResolveOpRefs(`EXEC "op://NOTKNOWN"`, ops)
	if err == nil {
		t.Fatal("expected error for unresolved op, got nil")
	}
	if !strings.Contains(err.Error(), "NOTKNOWN") {
		t.Errorf("error should mention the missing name, got: %v", err)
	}
	if !strings.Contains(err.Error(), "txco.yaml") {
		t.Errorf("error should hint at where to fix it (txco.yaml), got: %v", err)
	}
}

func TestResolveOpRefsCommentNotRewritten(t *testing.T) {
	ops := map[string]Operation{
		"CLASSIFY": {URL: "http://classify"},
	}
	// `op://FOO` inside a `#` comment is NOT a quoted literal, so the
	// regex shouldn't match it. The real EXEC reference still resolves.
	in := `# example: op://FOO is not a real op
EXEC "op://CLASSIFY"`
	got, err := ResolveOpRefs(in, ops)
	if err != nil {
		t.Fatalf("ResolveOpRefs: %v", err)
	}
	if !strings.Contains(got, "op://FOO") {
		t.Errorf("comment text 'op://FOO' should pass through, got: %q", got)
	}
	if !strings.Contains(got, `"http://classify"`) {
		t.Errorf("real EXEC reference should be resolved, got: %q", got)
	}
}

func TestResolveOpRefsUnquotedNotRewritten(t *testing.T) {
	ops := map[string]Operation{
		"CLASSIFY": {URL: "http://classify"},
	}
	// An unquoted op://CLASSIFY isn't a valid txcl operand, but if it
	// appears we shouldn't silently rewrite it — the developer should
	// see the original parse error.
	in := `# stray reference: op://CLASSIFY (unquoted)
EXEC "op://CLASSIFY"`
	got, err := ResolveOpRefs(in, ops)
	if err != nil {
		t.Fatalf("ResolveOpRefs: %v", err)
	}
	// The comment line should still mention op://CLASSIFY verbatim.
	lines := strings.Split(got, "\n")
	if !strings.Contains(lines[0], "op://CLASSIFY") {
		t.Errorf("unquoted occurrence should be preserved, got line: %q", lines[0])
	}
	if !strings.Contains(lines[1], `"http://classify"`) {
		t.Errorf("quoted occurrence should be resolved, got line: %q", lines[1])
	}
}

func TestResolveOpRefsNoRefs(t *testing.T) {
	ops := map[string]Operation{
		"CLASSIFY": {URL: "http://classify"},
	}
	in := `EXEC "http://example.com/already-real"`
	got, err := ResolveOpRefs(in, ops)
	if err != nil {
		t.Fatalf("ResolveOpRefs: %v", err)
	}
	if got != in {
		t.Errorf("rule with no op:// should pass through unchanged, got %q", got)
	}
}

func TestReferences(t *testing.T) {
	got := References(`EXEC "op://A" then SET .x = "op://B" but # op://C is in a comment
also "op://A" again`)
	if len(got) != 2 {
		t.Fatalf("got %d distinct refs, want 2: %v", len(got), got)
	}
	have := map[string]bool{}
	for _, n := range got {
		have[n] = true
	}
	if !have["A"] || !have["B"] {
		t.Errorf("expected refs to A and B, got %v", got)
	}
	if have["C"] {
		t.Errorf("comment-only ref C should not appear: %v", got)
	}
}

func TestHasRefs(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{`EXEC "op://X"`, true},
		{`EXEC "http://real"`, false},
		{`# op://X in comment only`, false},
		{``, false},
	}
	for _, tc := range cases {
		if got := HasRefs(tc.in); got != tc.want {
			t.Errorf("HasRefs(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestResolveOpRefsEmptyURLErrors(t *testing.T) {
	ops := map[string]Operation{
		"PARTIAL": {URL: ""}, // configured but no URL
	}
	_, err := ResolveOpRefs(`EXEC "op://PARTIAL"`, ops)
	if err == nil {
		t.Fatal("expected error for op with empty URL, got nil")
	}
}
