package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseDecl(t *testing.T) {
	d, err := ParseDecl([]byte("description: a deploy step\nenv:\n  GH_TOKEN: secret:GITHUB_PAT\n  GH_HOST: secret:GH_HOST\n  _again: secret:GITHUB_PAT\n"))
	if err != nil {
		t.Fatalf("valid decl: %v", err)
	}
	if d.Description != "a deploy step" || d.Env["GH_TOKEN"] != "secret:GITHUB_PAT" {
		t.Fatalf("decoded wrong: %+v", d)
	}
	if got := d.Vars(); !reflect.DeepEqual(got, []string{"GH_HOST", "GH_TOKEN", "_again"}) {
		t.Fatalf("Vars = %v", got)
	}
	if got := d.Secrets(); !reflect.DeepEqual(got, []string{"GH_HOST", "GITHUB_PAT"}) {
		t.Fatalf("Secrets = %v (want sorted, deduplicated)", got)
	}

	d, err = ParseDecl([]byte("env:\n  A: secret:B\n"))
	if err != nil || d.Description != "" {
		t.Fatalf("description is optional: %+v %v", d, err)
	}

	bad := map[string]string{
		"unknown key":      "env:\n  A: secret:B\nnetwork: [github.com]\n",
		"missing env":      "description: nothing\n",
		"empty env":        "env: {}\n",
		"bare value":       "env:\n  A: GITHUB_PAT\n",
		"unknown kind":     "env:\n  A: drive:doc_1\n",
		"bad secret name":  "env:\n  A: secret:github-pat\n",
		"empty secret":     "env:\n  A: 'secret:'\n",
		"bad variable":     "env:\n  GH-TOKEN: secret:B\n",
		"digit first":      "env:\n  1A: secret:B\n",
		"reserved":         "env:\n  TXCO_RUN_GRANT: secret:B\n",
		"env not a map":    "env: [A, B]\n",
		"long description": "description: " + strings.Repeat("x", MaxDescription+1) + "\nenv:\n  A: secret:B\n",
	}
	for name, body := range bad {
		if _, err := ParseDecl([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.HasPrefix(err.Error(), "sandbox declaration:") {
			t.Errorf("%s: error should be prefixed: %v", name, err)
		}
	}

	var many strings.Builder
	many.WriteString("env:\n")
	for i := 0; i <= MaxEnv; i++ {
		many.WriteString("  V" + strings.Repeat("x", i) + ": secret:B\n")
	}
	if _, err := ParseDecl([]byte(many.String())); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Errorf("over MaxEnv: %v", err)
	}
}

func TestParseRef(t *testing.T) {
	kind, name, err := ParseRef(" secret:GITHUB_PAT ")
	if err != nil || kind != KindSecret || name != "GITHUB_PAT" {
		t.Fatalf("ParseRef = %q %q %v", kind, name, err)
	}
	for _, bad := range []string{"", "GITHUB_PAT", "secret:", ":X", "secret:a:b", "drive:dc_1", "secret:1x"} {
		if _, _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q): expected an error", bad)
		}
	}
}

func TestPaths(t *testing.T) {
	cases := map[string]string{
		"SANDBOXES/github.yaml":      "github",
		"SANDBOXES/deploy_ro-2.yaml": "deploy_ro-2",
		"SANDBOXES/Github.yaml":      "",
		"SANDBOXES/a/b.yaml":         "",
		"SANDBOXES/github.json":      "",
		"SANDBOXES/.yaml":            "",
		"sandboxes/github.yaml":      "",
		"OUTLETS/github.yaml":        "",
	}
	for p, want := range cases {
		if got := Name(p); got != want {
			t.Errorf("Name(%q) = %q, want %q", p, got, want)
		}
	}
	if DeclPath("github") != "SANDBOXES/github.yaml" {
		t.Fatal("DeclPath")
	}
	if !IsSandboxPath("SANDBOXES/x") || IsSandboxPath("SANDBOXESX/x") {
		t.Fatal("IsSandboxPath")
	}
	for _, ok := range []string{"a", "github", "a-b_c9", strings.Repeat("a", 64)} {
		if !ValidName(ok) {
			t.Errorf("ValidName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "A", "1a", "a b", "a.b", strings.Repeat("a", 65)} {
		if ValidName(bad) {
			t.Errorf("ValidName(%q) = true", bad)
		}
	}
	if !ValidVar("_X9") || ValidVar("9X") || ValidVar("TXCO_RUN") || ValidVar("a-b") {
		t.Fatal("ValidVar")
	}
	if got := DeclNames(map[string]*Decl{"b": nil, "a": nil}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("DeclNames = %v", got)
	}
}

// A sandbox may name capabilities instead of, or as well as, variables.
func TestParseDeclCapabilities(t *testing.T) {
	d, err := ParseDecl([]byte("description: the workstation\ncapabilities:\n  - card.note\n  - ai.chat\n"))
	if err != nil {
		t.Fatalf("capabilities-only decl: %v", err)
	}
	if len(d.Env) != 0 || !reflect.DeepEqual(d.CapabilityNames(), []string{"ai.chat", "card.note"}) {
		t.Fatalf("decoded wrong: %+v", d)
	}
	d, err = ParseDecl([]byte("env:\n  A: secret:B\ncapabilities: [run.finish]\n"))
	if err != nil || d.Env["A"] != "secret:B" || !reflect.DeepEqual(d.CapabilityNames(), []string{"run.finish"}) {
		t.Fatalf("both: %+v %v", d, err)
	}
	for name, body := range map[string]string{
		"neither":    "description: nothing\n",
		"empty both": "env: {}\ncapabilities: []\n",
		"bad name":   "capabilities: [Card.Note]\n",
		"colon":      "capabilities: ['secret:X']\n",
		"twice":      "capabilities: [ai.chat, ai.chat]\n",
		"not a list": "capabilities: ai.chat\n",
	} {
		if _, err := ParseDecl([]byte(body)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

// TestParseDeclFiles: a sandbox serves files and a dotenv file beside its
// variables, references a connection as well as a secret, and snapshots as
// one map of entries the grant keeps.
func TestParseDeclFiles(t *testing.T) {
	d, err := ParseDecl([]byte("env:\n  GH_TOKEN: connection:github\nfiles:\n  ~/.ssh/id_ed25519: secret:DEPLOY_KEY\n  conf/app.key: secret:APP_KEY\ndotenv: run.env\n"))
	if err != nil {
		t.Fatalf("valid decl: %v", err)
	}
	want := map[string]string{
		"GH_TOKEN":               "connection:github",
		"file:~/.ssh/id_ed25519": "secret:DEPLOY_KEY",
		"file:conf/app.key":      "secret:APP_KEY",
		"dotenv:run.env":         DotenvValue,
	}
	if got := d.Entries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Entries = %v", got)
	}
	if got := d.Secrets(); !reflect.DeepEqual(got, []string{"APP_KEY", "DEPLOY_KEY"}) {
		t.Fatalf("Secrets = %v (files' secrets too, not a connection)", got)
	}
	for k, v := range want {
		if err := CheckEntry(k, v); err != nil {
			t.Errorf("CheckEntry(%q, %q): %v", k, v, err)
		}
	}
	if kind, p, ok := ParseEntry("file:~/.ssh/id_ed25519"); !ok || kind != EntryFile || p != "~/.ssh/id_ed25519" {
		t.Errorf("ParseEntry file = %q %q %v", kind, p, ok)
	}
	if _, err := ParseDecl([]byte("files:\n  key.pem: secret:K\n")); err != nil {
		t.Errorf("files alone are a sandbox: %v", err)
	}

	bad := map[string]string{
		"absolute path":       "files:\n  /etc/passwd: secret:K\n",
		"dot-dot":             "files:\n  ../up: secret:K\n",
		"dot-dot inside":      "files:\n  a/../b: secret:K\n",
		"home dot-dot":        "files:\n  ~/../root: secret:K\n",
		"space in path":       "files:\n  'a b': secret:K\n",
		"bare file value":     "files:\n  key: K\n",
		"bad provider":        "env:\n  A: connection:GitHub\n",
		"dotenv without env":  "files:\n  k: secret:K\ndotenv: run.env\n",
		"dotenv bad path":     "env:\n  A: secret:B\ndotenv: /tmp/run.env\n",
		"dotenv is a file":    "env:\n  A: secret:B\nfiles:\n  run.env: secret:K\ndotenv: run.env\n",
		"dotenv not a string": "env:\n  A: secret:B\ndotenv: [a]\n",
	}
	for name, body := range bad {
		if _, err := ParseDecl([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	for _, k := range []string{"file:/abs", "dotenv:../x", "GH-TOKEN", "other:x"} {
		if err := CheckEntry(k, "secret:K"); err == nil {
			t.Errorf("CheckEntry(%q) passed", k)
		}
	}
	if err := CheckEntry("dotenv:run.env", "secret:K"); err == nil {
		t.Error("a dotenv entry holds no reference")
	}
}
