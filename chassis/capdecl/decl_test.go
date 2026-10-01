package capdecl

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseDecl(t *testing.T) {
	d, err := ParseDecl([]byte("description: Send a message by email.\nentry: 7000\ninput:\n  to:\n    description: the recipient\n    required: true\n  subject: {}\n  message:\n    required: true\ntimeout: 60000\n"))
	if err != nil {
		t.Fatalf("valid decl: %v", err)
	}
	if d.Description != "Send a message by email." || d.Entry != 7000 || d.Timeout != 60000 {
		t.Fatalf("decoded wrong: %+v", d)
	}
	if !d.Input["to"].Required || d.Input["to"].Description != "the recipient" || d.Input["subject"].Required {
		t.Fatalf("input decoded wrong: %+v", d.Input)
	}
	if got := d.Params(); !reflect.DeepEqual(got, []string{"message", "subject", "to"}) {
		t.Fatalf("Params = %v", got)
	}
	if got := d.Stage("loop"); got != "loop/7000" {
		t.Fatalf("Stage = %q", got)
	}
	if got := d.TimeoutDuration(); got != time.Minute {
		t.Fatalf("TimeoutDuration = %v", got)
	}

	d, err = ParseDecl([]byte("entry: 2007\n"))
	if err != nil || d.Description != "" || len(d.Input) != 0 || d.TimeoutDuration() != 0 {
		t.Fatalf("description, input and timeout are optional: %+v %v", d, err)
	}
	if got := d.Params(); len(got) != 0 {
		t.Fatalf("Params of no input = %v", got)
	}

	bad := map[string]string{
		"unknown key":        "entry: 7000\noutput: {}\n",
		"missing entry":      "description: nothing\n",
		"zero entry":         "entry: 0\n",
		"negative entry":     "entry: -1\n",
		"entry not a number": "entry: seven\n",
		"entry a stage":      "entry: loop/7000\n",
		"bad input name":     "entry: 7000\ninput:\n  To: {}\n",
		"dashed input":       "entry: 7000\ninput:\n  fetch-url: {}\n",
		"input not a map":    "entry: 7000\ninput: [to, subject]\n",
		"unknown input key":  "entry: 7000\ninput:\n  to:\n    type: string\n",
		"negative timeout":   "entry: 7000\ntimeout: -5\n",
		"huge timeout":       "entry: 7000\ntimeout: 3600001\n",
		"long description":   "entry: 7000\ndescription: " + strings.Repeat("x", MaxDescription+1) + "\n",
		"long input note":    "entry: 7000\ninput:\n  to:\n    description: " + strings.Repeat("x", MaxInputDescription+1) + "\n",
	}
	for name, body := range bad {
		if _, err := ParseDecl([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.HasPrefix(err.Error(), "capability declaration:") {
			t.Errorf("%s: error should be prefixed: %v", name, err)
		}
	}

	var many strings.Builder
	many.WriteString("entry: 7000\ninput:\n")
	for i := 0; i <= MaxInputs; i++ {
		many.WriteString("  v" + strings.Repeat("x", i) + ": {}\n")
	}
	if _, err := ParseDecl([]byte(many.String())); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("over MaxInputs: %v", err)
	}
}

func TestPaths(t *testing.T) {
	cases := map[string]string{
		"CAPS/ai.chat.yaml":         "ai.chat",
		"CAPS/local.web.fetch.yaml": "local.web.fetch",
		"CAPS/finish.yaml":          "finish",
		"CAPS/Ai.Chat.yaml":         "",
		"CAPS/a/b.yaml":             "",
		"CAPS/ai.chat.json":         "",
		"CAPS/.yaml":                "",
		"CAPS/ai..chat.yaml":        "",
		"caps/ai.chat.yaml":         "",
		"SANDBOXES/ai.chat.yaml":    "",
	}
	for p, want := range cases {
		if got := Name(p); got != want {
			t.Errorf("Name(%q) = %q, want %q", p, got, want)
		}
	}
	if DeclPath("local.web.fetch") != "CAPS/local.web.fetch.yaml" {
		t.Fatal("DeclPath")
	}
	if !IsCapPath("CAPS/x") || IsCapPath("CAPSX/x") {
		t.Fatal("IsCapPath")
	}
	if !ValidInput("fetch_url") || ValidInput("Url") || ValidInput("9a") || ValidInput("a-b") || ValidInput("") {
		t.Fatal("ValidInput")
	}
}

func TestCheckEntry(t *testing.T) {
	d := &Decl{Entry: 7000}
	if err := CheckEntry(d, map[int]bool{100: true, 7000: true}); err != nil {
		t.Fatalf("entry names a scope: %v", err)
	}
	err := CheckEntry(d, map[int]bool{100: true})
	if err == nil || !strings.Contains(err.Error(), "entry 7000 names no scope") {
		t.Fatalf("entry with no scope: %v", err)
	}
	if err := CheckEntry(d, nil); err == nil {
		t.Fatal("no scopes at all: expected an error")
	}
}
