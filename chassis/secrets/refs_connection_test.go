package secrets

import "testing"

func TestParseRefsConnection(t *testing.T) {
	refs, err := ParseRefs(`{"secrets":{"headers":{"authorization":{"connection":"github"},"x-key":{"secret":"API_KEY","format":"k {}"}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
	secretRefs, connRefs := ConnectionRefs(refs)
	if len(secretRefs) != 1 || secretRefs[0].Secret != "API_KEY" || len(connRefs) != 1 || connRefs[0].Connection != "github" || connRefs[0].Path != "headers.authorization" || connRefs[0].Secret != "" {
		t.Errorf("split: %+v / %+v", secretRefs, connRefs)
	}
	if names := DistinctNames(refs); len(names) != 1 || names[0] != "API_KEY" {
		t.Errorf("DistinctNames = %v: a connection names no secret", names)
	}
	if opt := OptionalNames(refs); len(opt) != 1 {
		t.Errorf("OptionalNames = %v", opt)
	}
	refs, err = ParseRefs(`{"secrets":{"headers":{"authorization":{"connection":"github","format":"token {}"}}}}`)
	if err != nil || refs[0].Format != "token {}" {
		t.Errorf("format on a connection: %+v %v", refs, err)
	}
	for name, meta := range map[string]string{
		"both":         `{"secrets":{"headers":{"authorization":{"connection":"github","secret":"X"}}}}`,
		"empty":        `{"secrets":{"headers":{"authorization":{"connection":""}}}}`,
		"not a string": `{"secrets":{"headers":{"authorization":{"connection":1}}}}`,
		"at the root":  `{"secrets":{"connection":"github"}}`,
		"bad format":   `{"secrets":{"headers":{"authorization":{"connection":"github","format":"no slot"}}}}`,
	} {
		if _, err := ParseRefs(meta); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
