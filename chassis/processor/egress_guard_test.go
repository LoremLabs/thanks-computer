package processor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/open"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// guardedClient mirrors the transport New() builds: the egress Guard is
// enforced via net.Dialer.Control at the dial step.
func guardedClient(t *testing.T, policy string) *http.Client {
	t.Helper()
	g, err := egress.Open(policy, egress.Config{})
	if err != nil {
		t.Fatalf("egress.Open(%q): %v", policy, err)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout: 2 * time.Second,
		Control: egress.DialControl(g),
	}).DialContext
	return &http.Client{Transport: tr, Timeout: 2 * time.Second}
}

// TestExecHTTPEgressGuard proves the dial-step policy: a 127.0.0.1
// target is refused under "private" (ExecHTTP returns the existing
// dial-http-exec-err payload) and reachable under the default "open".
func TestExecHTTPEgressGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	op := operation.Operation{
		Input:     `{}`,
		Resonator: &resonator.Resonator{Exec: srv.URL},
	}

	// private: loopback target blocked at dial.
	puBlocked, _ := newTestUnit(t)
	puBlocked.HTTPClient = guardedClient(t, "private")
	payload, err := puBlocked.ExecHTTP(context.Background(), op)
	if err == nil {
		t.Fatalf("private policy: expected dial error to 127.0.0.1, got nil")
	}
	if !strings.Contains(payload.Meta, "dial-http-exec-err") {
		t.Errorf("private policy: expected dial-http-exec-err meta, got %q", payload.Meta)
	}

	// open: same target reachable, normal payload.
	puOpen, _ := newTestUnit(t)
	puOpen.HTTPClient = guardedClient(t, "open")
	if _, err := puOpen.ExecHTTP(context.Background(), op); err != nil {
		t.Fatalf("open policy: expected loopback reachable, got %v", err)
	}
}

// addrDenyGuard blocks one exact resolved "ip:port" and allows everything
// else. It lets the test exercise a cross-origin redirect where the entry
// host is permitted but the redirect target is not (both are 127.0.0.1
// under httptest, so a CIDR policy can't tell them apart — this isolates
// the redirect path itself).
type addrDenyGuard struct{ blocked string }

func (addrDenyGuard) Name() string { return "test-addr-deny" }
func (g addrDenyGuard) CheckAddr(_, address string) error {
	if address == g.blocked {
		return errBlockedRedirectTarget
	}
	return nil
}

var errBlockedRedirectTarget = &net.AddrError{Err: "blocked by test guard", Addr: "redirect-target"}

// TestExecHTTPEgressGuardBlocksRedirect proves a 30x to a disallowed host
// does NOT bypass the guard: the guard is consulted at the dial step of
// every redirect hop, so the second-hop dial is refused and the protected
// body never comes back.
func TestExecHTTPEgressGuardBlocksRedirect(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"secret":"metadata-creds"}`))
	}))
	t.Cleanup(internal.Close)

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, w2req(w), internal.URL+"/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(entry.Close)

	g := addrDenyGuard{blocked: strings.TrimPrefix(internal.URL, "http://")}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{
		Timeout: 2 * time.Second,
		Control: egress.DialControl(g),
	}).DialContext

	pu, _ := newTestUnit(t)
	pu.HTTPClient = &http.Client{Transport: tr, Timeout: 2 * time.Second}

	op := operation.Operation{
		Input:     `{}`,
		Resonator: &resonator.Resonator{Exec: entry.URL}, // allowed entry host
	}
	payload, err := pu.ExecHTTP(context.Background(), op)
	if err == nil {
		t.Fatalf("expected redirect-target dial to be blocked, got nil error")
	}
	if !strings.Contains(payload.Meta, "dial-http-exec-err") {
		t.Errorf("expected dial-http-exec-err meta, got %q", payload.Meta)
	}
	if strings.Contains(payload.Raw, "metadata-creds") {
		t.Fatalf("SSRF: protected body leaked through redirect: %q", payload.Raw)
	}
}

// TestExecRejectsNonNetworkSchemes confirms EXEC is a scheme allowlist:
// file:// (and other non-http schemes) are refused at dispatch — they
// never reach the dialer/egress guard and never touch the filesystem.
func TestExecRejectsNonNetworkSchemes(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secretPath, []byte("TOP-SECRET-FILE-BODY"), 0o600); err != nil {
		t.Fatalf("seed secret file: %v", err)
	}

	pu, _ := newTestUnit(t)

	for _, exec := range []string{
		"file://" + secretPath,
		"file:///etc/passwd",
		"gopher://127.0.0.1:70/",
		"ftp://127.0.0.1/x",
		"data:text/plain;base64,aGk=",
	} {
		op := operation.Operation{
			Input:     `{}`,
			Resonator: &resonator.Resonator{Exec: exec},
		}
		payload, _, err := pu.Exec(context.Background(), op)
		if err == nil {
			t.Errorf("%s: expected unsupported-scheme error, got nil", exec)
		} else if !strings.Contains(err.Error(), "unsupported EXEC value") {
			t.Errorf("%s: unexpected error %v", exec, err)
		}
		if strings.Contains(payload.Raw, "TOP-SECRET-FILE-BODY") ||
			strings.Contains(payload.Meta, "TOP-SECRET-FILE-BODY") {
			t.Fatalf("%s: file contents leaked into payload: raw=%q meta=%q", exec, payload.Raw, payload.Meta)
		}
	}
}

// w2req recovers the *http.Request from a ResponseWriter for
// http.Redirect, which needs the request only for relative-URL
// resolution; our target is absolute so a minimal request suffices.
func w2req(http.ResponseWriter) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	return r
}
