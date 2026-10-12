package calldecl

import (
	"strings"
	"testing"
)

func TestParseDecl(t *testing.T) {
	d, err := ParseDecl([]byte("description: Compile the context.\nentry: 100\ntimeout: 20000\ninput:\n  job:\n    required: true\n  text: {}\noutput:\n  prompt:\n    required: true\n  sources: {}\n"))
	if err != nil {
		t.Fatalf("valid decl: %v", err)
	}
	if d.Entry != 100 || d.Timeout != 20000 || !d.Input["job"].Required || d.Input["text"].Required ||
		!d.Output["prompt"].Required || d.Output["sources"].Required {
		t.Fatalf("decoded wrong: %+v", d)
	}
	if got := d.Returns(); len(got) != 2 || got[0] != "prompt" || got[1] != "sources" {
		t.Fatalf("Returns = %v", got)
	}
	if got := d.Stage("app/_orient"); got != "app/_orient/100" {
		t.Fatalf("Stage = %q", got)
	}
	// Errors name the kind of declaration.
	if _, err := ParseDecl([]byte("entry: -1\n")); err == nil || !strings.HasPrefix(err.Error(), "call declaration:") {
		t.Fatalf("prefix: %v", err)
	}
	if _, err := ParseDecl([]byte("output:\n  Prompt: {}\n")); err == nil || !strings.Contains(err.Error(), "output.Prompt") {
		t.Fatalf("output name rule: %v", err)
	}
	if err := CheckEntry(&Decl{Entry: 150}, map[int]bool{100: true}); err == nil || !strings.HasPrefix(err.Error(), "call declaration:") {
		t.Fatalf("CheckEntry: %v", err)
	}
}

func TestPaths(t *testing.T) {
	for p, want := range map[string]string{
		"CALLS/orient.yaml":                          "orient",
		"CALLS/send-mail.yaml":                       "send-mail",
		"CALLS/a_b.yaml":                             "a_b",
		"CALLS/Orient.yaml":                          "",
		"CALLS/a.b.yaml":                             "",
		"CALLS/a/b.yaml":                             "",
		"CALLS/orient.json":                          "",
		"CALLS/.yaml":                                "",
		"calls/orient.yaml":                          "",
		"CAPS/orient.yaml":                           "",
		"CALLSX/orient.yaml":                         "",
		"CALLS/" + strings.Repeat("a", 65) + ".yaml": "",
	} {
		if got := Name(p); got != want {
			t.Errorf("Name(%q) = %q, want %q", p, got, want)
		}
	}
	if DeclPath("orient") != "CALLS/orient.yaml" || !IsCallPath("CALLS/x") || IsCallPath("CALLSX/x") {
		t.Fatal("DeclPath / IsCallPath")
	}
	if !ValidName("orient") || !ValidName("a-b_c9") || ValidName("A") || ValidName("9a") || ValidName("a.b") || ValidName("") {
		t.Fatal("ValidName")
	}
}
