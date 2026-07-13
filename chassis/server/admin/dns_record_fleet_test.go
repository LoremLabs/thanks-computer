package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
)

// superRecordReq builds a record create/revoke request for a testTenant zone,
// carrying the super-admin context requireDNSZoneAccess demands by default and
// the {origin} mux var lookupTenantZone reads.
func superRecordReq(t *testing.T, method, origin string, body []byte, query string) *http.Request {
	t.Helper()
	target := "/v1/tenants/default/dns/zones/" + origin + "/records"
	if query != "" {
		target += "?" + query
	}
	r := muxRequest(method, target, body, map[string]string{"origin": origin})
	ac := &auth.Context{
		Source: "signed", ActorID: "actor_test", KeyID: "key_test",
		SuperAdmin: true, TenantID: testTenant, TenantSlug: "default",
	}
	return r.WithContext(auth.WithContext(r.Context(), ac))
}

// fleetDNSController returns a controller with the fleet producer enabled
// (feed sink + artifact store) and one verified zone for testTenant.
func fleetDNSController(t *testing.T, origin string) *Controller {
	t.Helper()
	c := newTestController(t, config.Config{
		Personalities:  "admin",
		DNSNameservers: []string{"ns1.txco.io", "ns2.txco.io"},
		FeedSink:       "file",
	})
	withAStore(t, c)
	rr := httptest.NewRecorder()
	c.handleCreateZone(rr, superZoneReq(t, origin))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create zone: status=%d body=%s", rr.Code, rr.Body.String())
	}
	return c
}

// recordOutboxRefs returns the artifact_refs of queued dns.record.upserted
// outbox rows, oldest first.
func recordOutboxRefs(t *testing.T, c *Controller) []string {
	t.Helper()
	rows, err := c.pu.RuntimeDB.Query(
		`SELECT artifact_ref FROM control_events_outbox WHERE event_type = ? ORDER BY id`,
		controlevent.TypeDNSRecordUpserted)
	if err != nil {
		t.Fatalf("outbox query: %v", err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			t.Fatalf("outbox scan: %v", err)
		}
		refs = append(refs, ref)
	}
	return refs
}

// artifactRow fetches ref from the test artifact store and returns its single
// dns_records row.
func artifactRow(t *testing.T, c *Controller, ref string) map[string]any {
	t.Helper()
	data, _, err := c.astore.Get(t.Context(), ref)
	if err != nil {
		t.Fatalf("artifact get %q: %v", ref, err)
	}
	var art controlevent.RowsArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		t.Fatalf("artifact decode: %v", err)
	}
	if art.DB != "runtime" || art.Table != "dns_records" || art.Op != "upsert" {
		t.Fatalf("artifact targets %s.%s op=%s, want runtime.dns_records upsert", art.DB, art.Table, art.Op)
	}
	if len(art.Rows) != 1 {
		t.Fatalf("artifact carries %d rows, want 1", len(art.Rows))
	}
	return art.Rows[0]
}

// TestRecordCreatePublishesFleetEvent: with the fleet producer enabled, a
// record create must queue a dns.record.upserted control event carrying the
// PERSISTED row ('@' apex, uppercased type, timestamps). Without it the write
// is admin-local and a dns head serves the zone minus its records — the
// driplit.co incident (todo-control-plane-reload-scaling.md addendum).
func TestRecordCreatePublishesFleetEvent(t *testing.T) {
	c := fleetDNSController(t, "ops.example.com")

	rr := httptest.NewRecorder()
	c.handleCreateRecord(rr, superRecordReq(t, http.MethodPost, "ops.example.com",
		mustJSON(t, map[string]any{"name": "", "type": "a", "rdata": "192.0.2.10"}), ""))
	if rr.Code != http.StatusCreated {
		t.Fatalf("create record: status=%d body=%s", rr.Code, rr.Body.String())
	}

	refs := recordOutboxRefs(t, c)
	if len(refs) != 1 {
		t.Fatalf("dns.record.upserted outbox rows = %d, want 1 (record not propagated to the fleet)", len(refs))
	}
	row := artifactRow(t, c, refs[0])
	if row["name"] != "@" || row["type"] != "A" {
		t.Fatalf("artifact row not normalized: name=%v type=%v, want @ A", row["name"], row["type"])
	}
	if row["rdata"] != "192.0.2.10" {
		t.Fatalf("artifact rdata=%v", row["rdata"])
	}
	if s, _ := row["created_at"].(string); s == "" {
		t.Fatalf("artifact missing created_at (persisted row must ride, not the request struct)")
	}
	if _, ok := row["revoked_at"]; ok {
		t.Fatalf("fresh record artifact carries revoked_at: %v", row["revoked_at"])
	}
}

// TestRecordRevokePublishesFleetEvent: revoking records must queue one
// dns.record.upserted per flipped row, each carrying revoked_at, so consumers
// stop serving them. (zone, name, type) is not unique — a multi-rdata set
// revokes as multiple rows.
func TestRecordRevokePublishesFleetEvent(t *testing.T) {
	c := fleetDNSController(t, "ops.example.com")

	for _, rdata := range []string{"10 mx1.example.com.", "20 mx2.example.com."} {
		rr := httptest.NewRecorder()
		c.handleCreateRecord(rr, superRecordReq(t, http.MethodPost, "ops.example.com",
			mustJSON(t, map[string]any{"name": "@", "type": "MX", "rdata": rdata}), ""))
		if rr.Code != http.StatusCreated {
			t.Fatalf("create record %q: status=%d body=%s", rdata, rr.Code, rr.Body.String())
		}
	}

	rr := httptest.NewRecorder()
	c.handleRevokeRecord(rr, superRecordReq(t, http.MethodDelete, "ops.example.com", nil, "name=@&type=mx"))
	if rr.Code != http.StatusOK {
		t.Fatalf("revoke: status=%d body=%s", rr.Code, rr.Body.String())
	}

	refs := recordOutboxRefs(t, c)
	if len(refs) != 4 {
		t.Fatalf("dns.record.upserted outbox rows = %d, want 4 (2 creates + 2 revokes)", len(refs))
	}
	// The two revoke events re-upload the same id-keyed artifacts, now with
	// revoked_at set (revocation travels as an upsert of the flipped row).
	for _, ref := range refs[2:] {
		row := artifactRow(t, c, ref)
		if s, _ := row["revoked_at"].(string); s == "" {
			t.Fatalf("revoke artifact %q missing revoked_at: %v", ref, row)
		}
	}
}
