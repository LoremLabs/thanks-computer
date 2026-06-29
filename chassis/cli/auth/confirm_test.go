package auth

import (
	"bytes"
	"strings"
	"testing"
)

func TestLocalChassis(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:8081":                true,
		"http://127.0.0.1:8081":                true,
		"http://[::1]:8081":                    true,
		"localhost:8081":                       true, // no scheme
		"http://www-5sllmgu3pa.localhost:8080": true, // dev minted host
		"":                                     true, // blank → not a remote
		"https://chassis.example.com":          false,
		"https://prod.thanks.computer":         false,
		"http://10.0.0.5:8081":                 false,
	}
	for in, want := range cases {
		if got := LocalChassis(in); got != want {
			t.Errorf("LocalChassis(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestConfirmTarget(t *testing.T) {
	const remote = "https://prod.example.com"
	const local = "http://localhost:8081"

	// Local chassis: never prompts, never errors, regardless of TTY.
	if err := ConfirmTarget("dev", local, false /*yes*/, false /*interactive*/, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("local chassis should not error: %v", err)
	}

	// Remote + --yes: skips the prompt.
	if err := ConfirmTarget("cloud", remote, true, false, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("--yes should skip prompt: %v", err)
	}

	// Remote + non-interactive + no --yes: fails closed.
	if err := ConfirmTarget("cloud", remote, false, false, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Fatal("non-interactive remote without --yes should fail closed")
	}

	// Remote + interactive + "y": proceeds.
	if err := ConfirmTarget("cloud", remote, false, true, strings.NewReader("y\n"), &bytes.Buffer{}); err != nil {
		t.Fatalf("interactive 'y' should proceed: %v", err)
	}

	// Remote + interactive + "n" (and empty default): aborts.
	for _, in := range []string{"n\n", "\n"} {
		if err := ConfirmTarget("cloud", remote, false, true, strings.NewReader(in), &bytes.Buffer{}); err == nil {
			t.Fatalf("interactive %q should abort", in)
		}
	}

	// Always announces the target (name + url) on stderr.
	var buf bytes.Buffer
	_ = ConfirmTarget("cloud", remote, true, false, strings.NewReader(""), &buf)
	if !strings.Contains(buf.String(), "cloud") || !strings.Contains(buf.String(), remote) {
		t.Errorf("expected target announcement, got %q", buf.String())
	}
}

func TestConfirmTargetT(t *testing.T) {
	const remote = "https://prod.example.com"

	// With a tenant, the banner shows it alongside the URL.
	var buf bytes.Buffer
	_ = ConfirmTargetT("cloud", remote, "prod-mankins", true, false, strings.NewReader(""), &buf)
	got := buf.String()
	if !strings.Contains(got, "cloud") || !strings.Contains(got, remote) || !strings.Contains(got, "tenant prod-mankins") {
		t.Errorf("expected name + url + tenant, got %q", got)
	}

	// Empty tenant keeps ConfirmTarget's output byte-for-byte (no "tenant" clause).
	var a, b bytes.Buffer
	_ = ConfirmTargetT("cloud", remote, "", true, false, strings.NewReader(""), &a)
	_ = ConfirmTarget("cloud", remote, true, false, strings.NewReader(""), &b)
	if a.String() != b.String() {
		t.Errorf("empty-tenant output drifted: %q vs %q", a.String(), b.String())
	}
	if strings.Contains(a.String(), "tenant") {
		t.Errorf("empty tenant should omit the clause, got %q", a.String())
	}

	// Tenant is cosmetic — a local chassis still never prompts.
	if err := ConfirmTargetT("dev", "http://localhost:8081", "default", false, false, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatalf("local chassis should not prompt even with a tenant: %v", err)
	}
}
