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

func insertZone(t *testing.T, c *Controller, id, origin, verifiedAt string) {
	t.Helper()
	var ver any
	if verifiedAt != "" {
		ver = verifiedAt
	}
	if _, err := c.pu.RuntimeDB.Exec(
		`INSERT INTO dns_zones (id, tenant_id, origin, mname, rname, created_at, updated_at, verified_at)
		 VALUES (?, 'tnt_default', ?, 'ns1.test.', 'hostmaster.test.', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', ?)`,
		id, origin, ver); err != nil {
		t.Fatalf("insert zone %q: %v", origin, err)
	}
}

// The print front door `ipp.<X>` gets a certificate when X is EXACTLY a
// verified zone origin or a verified, attached hostname — never for a
// subdomain of one, an unverified one, or anything under the structured
// suffix (that apex is on the wildcard cert).
func TestTLSAskIPPFrontDoor(t *testing.T) {
	c := newTestController(t, config.Config{
		Personalities:        "admin",
		StructuredHostSuffix: ".stacks.thanks.computer",
	})
	insertZone(t, c, "dz_ok", "dripl.example", "2026-05-19T00:00:00Z")
	insertZone(t, c, "dz_pending", "pending.example", "") // NS not verified yet
	insertHostname(t, c, "h_ok", "pony.acme.example", "shop", "", "2026-05-19T00:00:00Z")
	insertHostname(t, c, "h_unver", "unverified.acme.example", "shop", "", "")
	insertHostname(t, c, "h_struct", "web-ab2cd3.stacks.thanks.computer", "web", "", "2026-05-19T00:00:00Z")

	cases := map[string]int{
		"ipp.dripl.example":                     http.StatusOK,       // exact verified zone origin
		"IPP.Dripl.Example":                     http.StatusOK,       // canonicalized
		"ipp.sub.dripl.example":                 http.StatusNotFound, // covered by the zone, but not its origin
		"ipp.pending.example":                   http.StatusNotFound, // zone not verified
		"ipp.pony.acme.example":                 http.StatusOK,       // exact verified, attached hostname
		"ipp.unverified.acme.example":           http.StatusNotFound, // row not verified
		"ipp.never.seen.example":                http.StatusNotFound, // nothing behind it
		"ipp.stacks.thanks.computer":            http.StatusNotFound, // suffix apex: wildcard cert, never on-demand
		"ipp.web-ab2cd3.stacks.thanks.computer": http.StatusNotFound, // two labels under the suffix: never on-demand
		"notipp.dripl.example":                  http.StatusNotFound, // only the ipp label is special
		"dripl.example":                         http.StatusNotFound, // the rule authorizes ipp.<origin>, not the origin
	}
	for domain, want := range cases {
		if got := askStatus(t, c, domain); got != want {
			t.Errorf("ask(domain=%q) = %d, want %d", domain, got, want)
		}
	}
}
