package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

func TestBetterHostname(t *testing.T) {
	verified := client.Hostname{Hostname: "long-auto-host.stacks.example.com", VerifiedAt: "2026-01-01T00:00:00Z"}
	unverified := client.Hostname{Hostname: "app.acme.com"}
	shortVerified := client.Hostname{Hostname: "app.acme.com", VerifiedAt: "2026-01-01T00:00:00Z"}

	// Verified always beats unverified, even when the unverified name is shorter.
	if !betterHostname(verified, unverified) {
		t.Errorf("verified should beat unverified regardless of length")
	}
	if betterHostname(unverified, verified) {
		t.Errorf("unverified should not beat verified")
	}
	// Among equally-verified, the shorter (custom) name wins over the long auto-host.
	if !betterHostname(shortVerified, verified) {
		t.Errorf("shorter verified hostname should win")
	}
	// Stable tiebreak on identical length/verified-ness.
	a := client.Hostname{Hostname: "b.example.com", VerifiedAt: "x"}
	b := client.Hostname{Hostname: "a.example.com", VerifiedAt: "x"}
	if betterHostname(a, b) || !betterHostname(b, a) {
		t.Errorf("equal-length verified hostnames should tiebreak lexicographically")
	}
}

func TestPrintDriftTableWithURL(t *testing.T) {
	drifts := []stackDrift{
		{Stack: "hello", Remote: "v3", Local: "v3 (clean)", Note: "in sync", URL: "https://hello.example.com"},
		{Stack: "test-01", Remote: "v5", Local: "untracked",
			Note: "no local state recorded — run `txco pull test-01`", URL: "", Divergent: true},
	}
	var buf bytes.Buffer
	printDriftTable(&buf, drifts) // bytes.Buffer is not a TTY → no ANSI
	out := buf.String()

	if !strings.Contains(out, "url=https://hello.example.com") {
		t.Errorf("expected url= cell for the stack with a URL:\n%s", out)
	}
	// The note column still renders for the URL-less row, aligned past the
	// (blank) url cell.
	if !strings.Contains(out, "→ no local state recorded") {
		t.Errorf("URL-less row lost its note:\n%s", out)
	}
	if !strings.Contains(out, "→ in sync") {
		t.Errorf("missing in-sync note:\n%s", out)
	}
}

func TestPrintDriftTableNoURLUnchanged(t *testing.T) {
	// With no URLs anywhere, the url= column must not appear at all (keeps
	// `txco diff` and hostname-less chassis output byte-for-byte as before).
	drifts := []stackDrift{
		{Stack: "hello", Remote: "v3", Local: "v3 (clean)", Note: "in sync"},
	}
	var buf bytes.Buffer
	printDriftTable(&buf, drifts)
	if strings.Contains(buf.String(), "url=") {
		t.Errorf("url= column leaked when no stack has a URL:\n%s", buf.String())
	}
}

func TestDecorateStackURLs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/hostnames") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// hello: one verified custom + one verified long auto-host (shorter wins).
		// test-01: only unverified (still chosen — best available).
		// revoked + unattached rows must be ignored.
		_, _ = w.Write([]byte(`{"hostnames":[
			{"hostname":"hello-xyz.stacks.example.com","stack":"hello","verified_at":"2026-01-01T00:00:00Z"},
			{"hostname":"hello.example.com","stack":"hello","verified_at":"2026-01-01T00:00:00Z"},
			{"hostname":"test-01-abc.stacks.example.com","stack":"test-01"},
			{"hostname":"gone.example.com","stack":"hello","verified_at":"2026-01-01T00:00:00Z","revoked_at":"2026-02-01T00:00:00Z"},
			{"hostname":"floating.example.com","stack":""}
		]}`))
	}))
	defer srv.Close()

	c := client.New(client.Target{Addr: srv.URL, Tenant: "acme"})
	drifts := []stackDrift{
		{Stack: "hello"},
		{Stack: "test-01"},
		{Stack: "no-host"}, // no hostname row → stays empty
	}
	decorateStackURLs(context.Background(), c, drifts)

	if got := drifts[0].URL; got != "https://hello.example.com" {
		t.Errorf("hello URL = %q, want the shorter verified custom domain", got)
	}
	if got := drifts[1].URL; got != "https://test-01-abc.stacks.example.com" {
		t.Errorf("test-01 URL = %q, want the lone (unverified) host", got)
	}
	if got := drifts[2].URL; got != "" {
		t.Errorf("no-host URL = %q, want empty", got)
	}
}

func TestDecorateStackURLsBestEffort(t *testing.T) {
	// An unreachable chassis must not blow up status — URLs just stay empty.
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close()

	c := client.New(client.Target{Addr: dead, Tenant: "acme"})
	drifts := []stackDrift{{Stack: "hello"}}
	decorateStackURLs(context.Background(), c, drifts)
	if drifts[0].URL != "" {
		t.Errorf("expected empty URL on unreachable chassis, got %q", drifts[0].URL)
	}
}
