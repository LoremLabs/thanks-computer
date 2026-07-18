package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
)

// fakeHostnamesAdmin serves GET/POST /v1/tenants/default/hostnames with
// an in-memory row set and records POSTed binds.
type fakeHostnamesAdmin struct {
	rows  []map[string]string // hostname/stack rows returned by GET
	binds []map[string]string // bodies received by POST
}

func (f *fakeHostnamesAdmin) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/hostnames") {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"hostnames": f.rows})
		case http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.binds = append(f.binds, body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	})
}

func opsFor(stacks ...string) []bundle.Op {
	var ops []bundle.Op
	for _, s := range stacks {
		// Two ops per stack: distinctness must be by stack, not op count.
		ops = append(ops,
			bundle.Op{Stack: s, Scope: 50, Name: "a"},
			bundle.Op{Stack: s, Scope: 100, Name: "b"})
	}
	return ops
}

// wsWithBootScopes builds a temp workspace whose OPS/_sys/boot has the
// given scope directories (empty dirs suffice — the guard reads names).
func wsWithBootScopes(t *testing.T, scopes ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, s := range scopes {
		if err := os.MkdirAll(filepath.Join(dir, "OPS", "_sys", "boot", s), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func autoBindIn(t *testing.T, f *fakeHostnamesAdmin, dir string, ops []bundle.Op) string {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	resolved := ResolvedTarget{Chassis: srv.URL}
	return devAutoBindLocalhost(context.Background(), resolved, dir, ops, "http://localhost:18180", io.Discard)
}

func autoBind(t *testing.T, f *fakeHostnamesAdmin, ops []bundle.Op) string {
	t.Helper()
	return autoBindIn(t, f, wsWithBootScopes(t, "0", "50", "100", "1000"), ops)
}

// Exactly one non-underscore stack, localhost unbound ⇒ bind it.
// Underscore stacks (_llm, _sys/boot) don't count and don't disqualify.
func TestDevAutoBindLocalhostSingleStack(t *testing.T) {
	f := &fakeHostnamesAdmin{}
	got := autoBind(t, f, opsFor("app", "_llm", "_sys/boot"))
	if got != "app" {
		t.Fatalf("bound stack = %q, want app", got)
	}
	if len(f.binds) != 1 || f.binds[0]["hostname"] != "localhost" || f.binds[0]["stack"] != "app" {
		t.Fatalf("binds = %+v, want one localhost→app", f.binds)
	}
}

// Two web stacks ⇒ ambiguous ⇒ no bind (the manual tip stays).
func TestDevAutoBindLocalhostMultiStackSkips(t *testing.T) {
	f := &fakeHostnamesAdmin{}
	if got := autoBind(t, f, opsFor("app", "site")); got != "" {
		t.Fatalf("bound stack = %q, want none (ambiguous)", got)
	}
	if len(f.binds) != 0 {
		t.Fatalf("binds = %+v, want none", f.binds)
	}
}

// Only underscore stacks ⇒ nothing hostname-routable ⇒ no bind.
func TestDevAutoBindLocalhostUnderscoreOnlySkips(t *testing.T) {
	f := &fakeHostnamesAdmin{}
	if got := autoBind(t, f, opsFor("_llm", "_sys/boot")); got != "" {
		t.Fatalf("bound stack = %q, want none", got)
	}
}

// A custom boot scope (outside the vendored 0/50/100/1000 canon) marks a
// workspace that routes via its own boot hook — e.g. mcp-server's
// auto-route at 75 depends on unrouted requests falling through to the
// 404. No auto-bind, even with a single stack. Checked on the
// FILESYSTEM: bundle.Walk excludes the _sys tree, so the ops slice
// can't carry this signal.
func TestDevAutoBindLocalhostCustomBootHookSkips(t *testing.T) {
	f := &fakeHostnamesAdmin{}
	dir := wsWithBootScopes(t, "0", "50", "75", "100", "1000")
	if got := autoBindIn(t, f, dir, opsFor("mcp-server")); got != "" {
		t.Fatalf("bound stack = %q, want none (custom boot hook)", got)
	}
	if len(f.binds) != 0 {
		t.Fatalf("binds = %+v, want none", f.binds)
	}
	// Labeled scope dirs count by numeric prefix, like the OPS walker.
	dir = wsWithBootScopes(t, "0", "50", "75_auto-route", "100", "1000")
	if got := autoBindIn(t, f, dir, opsFor("mcp-server")); got != "" {
		t.Fatalf("bound stack = %q, want none (labeled custom hook)", got)
	}
}

// The vendored boot canon (0/50/100/1000) does NOT disqualify — that's
// every plain workspace. Neither does a missing OPS/_sys/boot entirely.
func TestDevAutoBindLocalhostCanonicalBootStillBinds(t *testing.T) {
	f := &fakeHostnamesAdmin{}
	if got := autoBind(t, f, opsFor("app")); got != "app" {
		t.Fatalf("bound stack = %q, want app", got)
	}
	f2 := &fakeHostnamesAdmin{}
	if got := autoBindIn(t, f2, t.TempDir(), opsFor("app")); got != "app" {
		t.Fatalf("no-boot-dir workspace: bound stack = %q, want app", got)
	}
}

// An existing localhost row is respected, never rewritten — but its
// stack is reported so the caller suppresses the bind tip.
func TestDevAutoBindLocalhostExistingRowRespected(t *testing.T) {
	f := &fakeHostnamesAdmin{rows: []map[string]string{
		{"hostname": "localhost", "stack": "other"},
	}}
	got := autoBind(t, f, opsFor("app"))
	if got != "other" {
		t.Fatalf("reported stack = %q, want the existing binding's (other)", got)
	}
	if len(f.binds) != 0 {
		t.Fatalf("binds = %+v, want none (existing row wins)", f.binds)
	}
}

// A claimed-but-unattached localhost row (Vercel-style decoupled claim)
// is the user's arrangement: no bind, no report.
func TestDevAutoBindLocalhostUnattachedRowLeft(t *testing.T) {
	f := &fakeHostnamesAdmin{rows: []map[string]string{
		{"hostname": "localhost", "stack": ""},
	}}
	if got := autoBind(t, f, opsFor("app")); got != "" {
		t.Fatalf("reported stack = %q, want none", got)
	}
	if len(f.binds) != 0 {
		t.Fatalf("binds = %+v, want none", f.binds)
	}
}
