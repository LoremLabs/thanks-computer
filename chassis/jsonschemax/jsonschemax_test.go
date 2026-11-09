package jsonschemax

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return v
}

func TestValidateOK(t *testing.T) {
	ok, probs, err := Validate([]byte(`{"type":"object","properties":{"n":{"type":"integer","minimum":1}},"required":["n"],"additionalProperties":false}`), decode(t, `{"n":3}`))
	if err != nil || !ok || len(probs) != 0 {
		t.Fatalf("ok=%v probs=%v err=%v", ok, probs, err)
	}
}

func TestCheckReportsLeavesWithPaths(t *testing.T) {
	sch, err := Compile([]byte(`{"type":"object","properties":{"n":{"type":"integer","minimum":1},"q":{"type":"string"}},"required":["n","q"],"additionalProperties":false}`))
	if err != nil {
		t.Fatal(err)
	}
	probs := Check(sch, decode(t, `{"n":"x","extra":1}`))
	if len(probs) == 0 {
		t.Fatal("want problems")
	}
	var paths []string
	for _, p := range probs {
		paths = append(paths, p.Path)
		if p.Message == "" || strings.Contains(p.Message, "doesn't validate") {
			t.Errorf("leaf problem has a wrapper message: %+v", p)
		}
	}
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "/n") {
		t.Errorf("want a problem at /n (type), got %v", probs)
	}
	// The missing field and the extra one are reported on the object itself.
	if !strings.Contains(joined, "") {
		t.Errorf("want a root problem, got %v", probs)
	}
	if len(probs) < 3 {
		t.Errorf("want type, missing and additional-property problems, got %v", probs)
	}
}

func TestCompileRefusesRemoteRefs(t *testing.T) {
	for _, ref := range []string{"file:///etc/hosts", "https://example.com/s.json", "/etc/hosts"} {
		_, err := Compile([]byte(`{"type":"object","properties":{"x":{"$ref":"` + ref + `"}}}`))
		if err == nil {
			t.Fatalf("%s: compiled; want a refusal", ref)
		}
		if !errors.Is(err, ErrRemoteRef) {
			t.Errorf("%s: err = %v, want ErrRemoteRef", ref, err)
		}
	}
}

func TestCompileAllowsLocalRefs(t *testing.T) {
	sch, err := Compile([]byte(`{"$defs":{"id":{"type":"string","minLength":1}},"type":"object","properties":{"a":{"$ref":"#/$defs/id"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if probs := Check(sch, decode(t, `{"a":""}`)); len(probs) != 1 || probs[0].Path != "/a" {
		t.Errorf("probs = %v, want one at /a", probs)
	}
}

func TestCompileBounds(t *testing.T) {
	if _, err := Compile([]byte("  ")); !errors.Is(err, ErrEmpty) {
		t.Errorf("empty: err = %v", err)
	}
	big := `{"type":"object","description":"` + strings.Repeat("x", MaxSchemaBytes) + `"}`
	if _, err := Compile([]byte(big)); !errors.Is(err, ErrTooLarge) {
		t.Errorf("large: err = %v", err)
	}
	if _, err := Compile([]byte(`{"type":`)); err == nil {
		t.Error("malformed JSON compiled")
	}
	if _, err := Compile([]byte(`{"type":"nope"}`)); err == nil {
		t.Error("invalid keyword value compiled")
	}
}

func TestDefaultDraftIs2020(t *testing.T) {
	// prefixItems exists only from draft 2020-12; under an older draft it is
	// ignored and the second element would pass.
	ok, _, err := Validate([]byte(`{"type":"array","prefixItems":[{"type":"integer"},{"type":"string"}]}`), decode(t, `[1, 2]`))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("prefixItems not enforced: the default draft is older than 2020-12")
	}
}
