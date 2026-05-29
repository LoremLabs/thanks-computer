package controlevent

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestJSONRoundTrip(t *testing.T) {
	in := Event{
		ID:             12346,
		EventID:        "01HXYZ12345EVENTABC",
		Type:           TypeStackActivated,
		TenantID:       "t_123",
		StackID:        "web",
		Version:        42,
		BaseVersion:    41,
		ArtifactRef:    "file://txco-stacks/t_123/web/42.snap",
		Checksum:       "sha256:abc",
		ControlVersion: 1234,
		CreatedAt:      "2026-05-16T00:00:00Z",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The contract field is artifact_ref, never snapshot_ref, and there is
	// no active_version. event_id is always present.
	s := string(b)
	if !contains(s, `"artifact_ref"`) {
		t.Errorf("missing artifact_ref in %s", s)
	}
	if !contains(s, `"event_id"`) {
		t.Errorf("missing event_id in %s", s)
	}
	if contains(s, "snapshot_ref") || contains(s, "active_version") {
		t.Errorf("unexpected legacy field in %s", s)
	}
	var out Event
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out != in {
		t.Errorf("round-trip mismatch:\n in=%+v\nout=%+v", in, out)
	}
}

func TestValidate(t *testing.T) {
	ok := Event{
		EventID: "evt_1", Type: TypeStackActivated,
		TenantID: "t", StackID: "s", Version: 1, ControlVersion: 5,
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid event rejected: %v", err)
	}

	cases := []struct {
		name string
		e    Event
	}{
		{"empty event_id", Event{Type: TypeTenantCreated, ControlVersion: 1}},
		{"empty type", Event{EventID: "evt_1", ControlVersion: 1}},
		{"unknown type", Event{EventID: "evt_1", Type: "bogus.thing", ControlVersion: 1}},
		{"zero control_version", Event{EventID: "evt_1", Type: TypeTenantCreated}},
		{"bad checksum prefix", Event{EventID: "evt_1", Type: TypeTenantCreated, ControlVersion: 1, Checksum: "md5:x"}},
		{"artifact without checksum", Event{EventID: "evt_1", Type: TypeTenantCreated, ControlVersion: 1, ArtifactRef: "file://x"}},
		{"stack.activated missing fields", Event{EventID: "evt_1", Type: TypeStackActivated, ControlVersion: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.e.Validate()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("error not wrapping ErrInvalid: %v", err)
			}
		})
	}
}

func TestVersionCompat(t *testing.T) {
	if !CompatibleModel(ControlModelVersion) {
		t.Errorf("current model version must be compatible with itself")
	}
	if CompatibleModel(ControlModelVersion + 1) {
		t.Errorf("a newer model version must NOT be silently accepted")
	}
	if !CompatibleCacheSchema(CacheSchemaVersion) {
		t.Errorf("current cache schema must be compatible with itself")
	}
	if CompatibleCacheSchema(CacheSchemaVersion + 1) {
		t.Errorf("a newer cache schema must NOT be silently accepted")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
