package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

func insertHostname(t *testing.T, c *Controller, id, host, stack, revokedAt, verifiedAt string) {
	t.Helper()
	var rev, ver any
	if revokedAt != "" {
		rev = revokedAt
	}
	if verifiedAt != "" {
		ver = verifiedAt
	}
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO tenant_hostnames
		   (id, hostname, tenant_id, stack, created_at, created_by, revoked_at, verified_at)
		 VALUES (?, ?, 'tnt_default', ?, '2026-01-01T00:00:00Z', 'system:test', ?, ?)`,
		id, host, stack, rev, ver); err != nil {
		t.Fatalf("insert hostname %q: %v", host, err)
	}
}

func askStatus(t *testing.T, c *Controller, domain string) int {
	t.Helper()
	u := tlsAskPath
	if domain != "\x00" { // sentinel: omit the param entirely
		u += "?domain=" + url.QueryEscape(domain)
	}
	rr := httptest.NewRecorder()
	c.handleTLSAsk(rr, httptest.NewRequest(http.MethodGet, u, nil))
	return rr.Code
}

func TestTLSAskAuthorizesOnlyVerifiedAttachedActive(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	insertHostname(t, c, "h_ok", "app.acme.example", "shop", "", "2026-05-19T00:00:00Z")
	insertHostname(t, c, "h_unver", "unverified.acme.example", "shop", "", "")                 // verified_at NULL
	insertHostname(t, c, "h_unatt", "unattached.acme.example", "", "", "2026-05-19T00:00:00Z") // stack=''
	insertHostname(t, c, "h_rev", "revoked.acme.example", "shop", "2026-05-19T01:00:00Z", "2026-05-19T00:00:00Z")

	cases := map[string]int{
		"app.acme.example":        http.StatusOK,         // verified+attached+active
		"APP.acme.example":        http.StatusOK,         // canonicalized (lowercased)
		"unverified.acme.example": http.StatusNotFound,   // not verified → no cert
		"unattached.acme.example": http.StatusNotFound,   // no stack
		"revoked.acme.example":    http.StatusNotFound,   // revoked
		"never.seen.example":      http.StatusNotFound,   // absent
		"":                        http.StatusBadRequest, // empty domain value
		"\x00":                    http.StatusBadRequest, // ?domain omitted entirely
		"not a hostname":          http.StatusNotFound,   // invalid
	}
	for domain, want := range cases {
		if got := askStatus(t, c, domain); got != want {
			t.Errorf("ask(domain=%q) = %d, want %d", domain, got, want)
		}
	}
}

// Names under the structured-host apex are served by the *.stacks
// wildcard cert and must be denied here even though they carry a
// verified, attached, active row.
func TestTLSAskDeniesStructuredHostSuffix(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".stacks.thanks.computer",
	})
	insertHostname(t, c, "h_struct", "test-stack-ab2cd3.stacks.thanks.computer", "test-stack", "", "2026-05-19T00:00:00Z")
	insertHostname(t, c, "h_cust", "app.acme.example", "shop", "", "2026-05-19T00:00:00Z")

	if got := askStatus(t, c, "test-stack-ab2cd3.stacks.thanks.computer"); got != http.StatusNotFound {
		t.Fatalf("structured-suffix host: got %d, want 404 (served by wildcard cert, not on-demand)", got)
	}
	if got := askStatus(t, c, "app.acme.example"); got != http.StatusOK {
		t.Fatalf("custom host alongside suffix guard: got %d, want 200", got)
	}
}
