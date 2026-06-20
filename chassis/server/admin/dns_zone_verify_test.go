package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
)

// superZoneReq builds a POST /dns/zones request carrying a super-admin context
// for testTenant (requireDNSZoneAccess is super-admin-only by default).
func superZoneReq(t *testing.T, origin string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/dns/zones",
		bytes.NewReader(mustJSON(t, map[string]any{"origin": origin})))
	ac := &auth.Context{
		Source: "signed", ActorID: "actor_test", KeyID: "key_test",
		SuperAdmin: true, TenantID: testTenant, TenantSlug: "default",
	}
	return r.WithContext(auth.WithContext(r.Context(), ac))
}

// TestCreateZoneVerificationFlag: with --dns-require-zone-verification off
// (default) a created zone is verified immediately (current behavior); with it
// on, the zone is pending and the delegation hint points at `zone verify`.
func TestCreateZoneVerificationFlag(t *testing.T) {
	ns := []string{"ns1.txco.io", "ns2.txco.io"}

	t.Run("flag off auto-verifies (no regression)", func(t *testing.T) {
		c := newTestController(t, config.Config{Personalities: "admin", DNSNameservers: ns})
		rr := httptest.NewRecorder()
		c.handleCreateZone(rr, superZoneReq(t, "ops.example.com"))
		if rr.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var resp createZoneResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Zone.VerifiedAt == "" {
			t.Errorf("flag off: zone should be verified at creation, got pending: %+v", resp.Zone)
		}
	})

	t.Run("flag on leaves pending", func(t *testing.T) {
		c := newTestController(t, config.Config{Personalities: "admin",
			DNSNameservers: ns, DNSRequireZoneVerification: true})
		rr := httptest.NewRecorder()
		c.handleCreateZone(rr, superZoneReq(t, "ops.example.com"))
		if rr.Code != http.StatusCreated {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var resp createZoneResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Zone.VerifiedAt != "" {
			t.Errorf("flag on: zone should be pending, got verified: %+v", resp.Zone)
		}
		if !strings.Contains(resp.Delegation, "zone verify") {
			t.Errorf("flag on: delegation should point at `zone verify`, got: %q", resp.Delegation)
		}
	})
}
