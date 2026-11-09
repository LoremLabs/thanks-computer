package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

const nestedSchema = `{
  "type": "object",
  "properties": {
    "q": {"type": "string", "description": "the query"},
    "count": {"type": "integer", "minimum": 1, "default": 10},
    "options": {"type": "object", "properties": {"limit": {"type": "integer"}, "deep": {"type": "object", "properties": {"flag": {"type": "boolean"}}}}},
    "items": {"type": "array", "items": {"type": "object", "properties": {"label": {"type": "string"}, "n": {"type": "integer"}}, "required": ["n"]}},
    "maybe": {"type": ["string", "null"]}
  },
  "required": ["q", "maybe"]
}`

func TestQualifies(t *testing.T) {
	if ok, why := Qualifies(json.RawMessage(nestedSchema)); !ok {
		t.Fatalf("nested schema should qualify: %s", why)
	}
	bad := map[string]string{
		"untyped property":          `{"type":"object","properties":{"q":{"description":"anything"}}}`,
		"additionalProperties true": `{"type":"object","properties":{"q":{"type":"string"}},"additionalProperties":true}`,
		"optional and nullable":     `{"type":"object","properties":{"q":{"type":["string","null"]}}}`,
		"optional enum with null":   `{"type":"object","properties":{"q":{"enum":["a",null]}}}`,
		"$ref":                      `{"type":"object","properties":{"q":{"$ref":"#/$defs/x"}},"$defs":{"x":{"type":"string"}}}`,
		"oneOf":                     `{"type":"object","properties":{"q":{"oneOf":[{"type":"string"},{"type":"integer"}]}}}`,
		"anyOf of objects":          `{"type":"object","properties":{"q":{"anyOf":[{"type":"object"},{"type":"string"}]}}}`,
		"root not object":           `{"type":"array","items":{"type":"string"}}`,
		"untyped nested":            `{"type":"object","properties":{"o":{"type":"object","properties":{"x":{}}}}}`,
		"untyped items":             `{"type":"object","properties":{"a":{"type":"array","items":{}}}}`,
		"not json":                  `[1]`,
	}
	for name, s := range bad {
		if ok, why := Qualifies(json.RawMessage(s)); ok {
			t.Errorf("%s: qualified", name)
		} else if why == "" {
			t.Errorf("%s: no reason", name)
		}
	}
	// A required nullable property is fine (null is a value there), and so
	// is an anyOf of scalars.
	for name, s := range map[string]string{
		"required nullable": `{"type":"object","properties":{"q":{"type":["string","null"]}},"required":["q"]}`,
		"scalar anyOf":      `{"type":"object","properties":{"part":{"anyOf":[{"type":"integer"},{"type":"string","enum":["last"]}]}},"required":["part"]}`,
		"empty object":      `{"type":"object"}`,
	} {
		if ok, why := Qualifies(json.RawMessage(s)); !ok {
			t.Errorf("%s: %s", name, why)
		}
	}
}

func TestMakeStrictWrapsOptionals(t *testing.T) {
	out, wrapped, err := MakeStrict(json.RawMessage(nestedSchema))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/count", "/items", "/items/-/label", "/options", "/options/deep", "/options/deep/flag", "/options/limit"}
	if strings.Join(wrapped, " ") != strings.Join(want, " ") {
		t.Errorf("wrapped = %v, want %v", wrapped, want)
	}
	var s map[string]any
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	// Every property required, in sorted order; the object closed.
	req := s["required"].([]any)
	if len(req) != 5 || req[0] != "count" || req[4] != "q" || s["additionalProperties"] != false {
		t.Errorf("root required/additional = %v %v", s["required"], s["additionalProperties"])
	}
	props := s["properties"].(map[string]any)
	// An optional field is wrapped and keeps its annotations inside the wrap.
	count := props["count"].(map[string]any)["anyOf"].([]any)
	if count[0].(map[string]any)["default"] != float64(10) || count[1].(map[string]any)["type"] != "null" {
		t.Errorf("count = %v", props["count"])
	}
	// A required field is left alone, nullable or not.
	if _, wrappedQ := props["q"].(map[string]any)["anyOf"]; wrappedQ {
		t.Error("required q was wrapped")
	}
	if typ := props["maybe"].(map[string]any)["type"].([]any); len(typ) != 2 {
		t.Errorf("maybe = %v", props["maybe"])
	}
	// Nested objects are closed and their optionals wrapped, through the
	// outer wrap; array items too.
	options := props["options"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)
	if options["additionalProperties"] != false || len(options["required"].([]any)) != 2 {
		t.Errorf("options = %v", options)
	}
	items := props["items"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["items"].(map[string]any)
	if items["additionalProperties"] != false || len(items["required"].([]any)) != 2 {
		t.Errorf("items = %v", items)
	}
	if _, wrappedN := items["properties"].(map[string]any)["n"].(map[string]any)["anyOf"]; wrappedN {
		t.Error("required n inside items was wrapped")
	}
	// The input is not mutated, and a non-qualifying schema is refused.
	if !strings.Contains(nestedSchema, `"required": ["q", "maybe"]`) {
		t.Error("input changed")
	}
	if _, _, err := MakeStrict(json.RawMessage(`{"type":"object","properties":{"q":{}}}`)); err == nil {
		t.Error("non-qualifying schema transformed")
	}
}

func TestStripWrappedNullsOnlyWrapped(t *testing.T) {
	_, wrapped, err := MakeStrict(json.RawMessage(nestedSchema))
	if err != nil {
		t.Fatal(err)
	}
	doc := json.RawMessage(`{"q":"tides","count":null,"maybe":null,"options":{"limit":null,"deep":{"flag":null}},"items":[{"label":null,"n":1},{"label":"x","n":2}]}`)
	got := string(StripWrappedNulls(doc, wrapped))
	want := `{"items":[{"n":1},{"label":"x","n":2}],"maybe":null,"options":{"deep":{}},"q":"tides"}`
	if got != want {
		t.Errorf("stripped = %s\nwant      %s", got, want)
	}
	// Nothing to strip: unchanged bytes. Not JSON: unchanged bytes.
	if string(StripWrappedNulls(doc, nil)) != string(doc) {
		t.Error("nil wrapped changed the doc")
	}
	if string(StripWrappedNulls(json.RawMessage(`{not json`), wrapped)) != `{not json` {
		t.Error("bad doc changed")
	}
	// Pointer escapes: a key with a slash.
	_, w2, _ := MakeStrict(json.RawMessage(`{"type":"object","properties":{"a/b":{"type":"string"}}}`))
	if len(w2) != 1 || w2[0] != "/a~1b" {
		t.Errorf("escaped pointer = %v", w2)
	}
	if got := string(StripWrappedNulls(json.RawMessage(`{"a/b":null,"c":null}`), w2)); got != `{"c":null}` {
		t.Errorf("escaped strip = %s", got)
	}
}
