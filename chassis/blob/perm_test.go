package blob

import (
	"reflect"
	"testing"
)

func TestValidRequirement(t *testing.T) {
	for _, p := range []string{"blob:guest:read", "blob:internal:read", "blob:cas:read", "blob:ops.eu:write"} {
		if err := ValidRequirement(p); err != nil {
			t.Errorf("ValidRequirement(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"", "blob:guest", "blob:*:read", "blob:guest:*", "kv:guest:read", "blob::read", "blob:a b:read", "admin:all", "*"} {
		if err := ValidRequirement(p); err == nil {
			t.Errorf("ValidRequirement(%q) = nil, want error", p)
		}
	}
}

func TestSubjectGrantsAndAllowed(t *testing.T) {
	g, err := SubjectGrants("guest", []string{" blob:cas:read ", "blob:guest:read", ""})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"blob:cas:read", "blob:guest:read"}; !reflect.DeepEqual(g, want) {
		t.Fatalf("SubjectGrants = %v, want %v (sorted, deduped)", g, want)
	}
	if _, err := SubjectGrants("gue st", nil); err == nil {
		t.Error("bad audience accepted")
	}
	if _, err := SubjectGrants("", []string{"blob:guest"}); err == nil {
		t.Error("2-segment grant accepted")
	}

	guest := []string{"blob:guest:read"}
	if !Allowed(guest, nil) {
		t.Error("empty requirement must be open")
	}
	if !Allowed(guest, []string{"blob:guest:read"}) {
		t.Error("exact cover failed")
	}
	if Allowed(guest, []string{"blob:internal:read"}) {
		t.Error("guest covered internal")
	}
	// AND semantics: both requirements must be covered.
	if Allowed(guest, []string{"blob:guest:read", "blob:internal:read"}) {
		t.Error("partial cover passed")
	}
	if !Allowed([]string{"blob:*:read"}, []string{"blob:guest:read", "blob:internal:read"}) {
		t.Error("wildcard grant did not cover")
	}
	if CanReadByHash(guest) {
		t.Error("guest can read by hash")
	}
	if !CanReadByHash([]string{"blob:*:*"}) || !CanReadByHash([]string{CASRead}) {
		t.Error("cas read not covered")
	}
}
