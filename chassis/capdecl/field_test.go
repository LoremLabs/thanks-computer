package capdecl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/jsonschemax"
)

const typedDecl = `
description: Search the public web.
input:
  query:
    type: string
    max_length: 500
    description: a few words
    required: true
  kind:
    type: string
    enum: [web, news]
    default: web
  count:
    type: integer
    minimum: 1
    maximum: 20
    default: 10
  tags:
    type: array
    items: {type: string, min_length: 1}
  options:
    type: object
    fields:
      verbose: {type: boolean, description: say more}
      depth: {type: integer, required: true}
  part:
    description: which part, or "last"
    schema: {anyOf: [{type: integer, minimum: 1}, {type: string, enum: [last]}]}
  note: {}
  maybe:
    type: [string, "null"]
    required: true
timeout: 30000
`

func TestFieldTypesParse(t *testing.T) {
	d, err := ParseDecl([]byte(typedDecl))
	if err != nil {
		t.Fatal(err)
	}
	q := d.Input["query"]
	if !q.Type.Has("string") || q.MaxLength == nil || *q.MaxLength != 500 || !q.Required {
		t.Errorf("query = %+v", q)
	}
	if k := d.Input["kind"]; len(k.Enum) != 2 || k.Default != "web" {
		t.Errorf("kind = %+v", k)
	}
	if c := d.Input["count"]; c.Minimum == nil || *c.Minimum != 1 || c.Default != 10 {
		t.Errorf("count = %+v", c)
	}
	if tg := d.Input["tags"]; tg.Items == nil || !tg.Items.Type.Has("string") {
		t.Errorf("tags = %+v", tg)
	}
	if o := d.Input["options"]; o.Fields["depth"].Required != true || !o.Fields["verbose"].Type.Has("boolean") {
		t.Errorf("options = %+v", o)
	}
	if p := d.Input["part"]; p.Schema == nil || p.Typed() == false {
		t.Errorf("part = %+v", p)
	}
	if n := d.Input["note"]; n.Typed() {
		t.Errorf("note should be untyped: %+v", n)
	}
	if m := d.Input["maybe"]; !m.Type.Has("null") || len(m.Type) != 2 {
		t.Errorf("maybe = %+v", m)
	}
	// JSON of a field: type as a string when one, an array when several.
	b, _ := json.Marshal(d.Input["query"])
	if !strings.Contains(string(b), `"type":"string"`) || !strings.Contains(string(b), `"max_length":500`) {
		t.Errorf("field json = %s", b)
	}
	b, _ = json.Marshal(d.Input["maybe"])
	if !strings.Contains(string(b), `"type":["string","null"]`) {
		t.Errorf("field json = %s", b)
	}
	var back Field
	if err := json.Unmarshal(b, &back); err != nil || !back.Type.Has("null") {
		t.Errorf("field json round trip: %v %+v", err, back)
	}
}

func TestFieldUnknownKeyStillRefused(t *testing.T) {
	for name, body := range map[string]string{
		"typo at top":    "input:\n  q:\n    typo: 1\n",
		"typo in fields": "input:\n  o:\n    type: object\n    fields:\n      x:\n        typo: 1\n",
		"typo in items":  "input:\n  a:\n    type: array\n    items:\n      typo: 1\n",
	} {
		if _, err := ParseDecl([]byte(body)); err == nil || !strings.Contains(err.Error(), "typo") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestFieldRules(t *testing.T) {
	bad := map[string]string{
		"unknown type":          "input:\n  q: {type: text}\n",
		"type not scalar":       "input:\n  q:\n    type: {a: 1}\n",
		"items without array":   "input:\n  q:\n    type: string\n    items: {type: string}\n",
		"fields without object": "input:\n  q:\n    type: string\n    fields: {a: {}}\n",
		"minimum on string":     "input:\n  q:\n    type: string\n    minimum: 1\n",
		"min above max":         "input:\n  q:\n    type: integer\n    minimum: 5\n    maximum: 1\n",
		"pattern on integer":    "input:\n  q:\n    type: integer\n    pattern: x\n",
		"bad pattern":           "input:\n  q:\n    type: string\n    pattern: '(?=x)'\n",
		"negative min_length":   "input:\n  q:\n    type: string\n    min_length: -1\n",
		"schema with type":      "input:\n  q:\n    type: string\n    schema: {type: string}\n",
		"schema not compiling":  "input:\n  q:\n    schema: {type: nope}\n",
		"schema remote ref":     "input:\n  q:\n    schema: {$ref: 'file:///etc/hosts'}\n",
		"bad nested name":       "input:\n  o:\n    type: object\n    fields:\n      Bad: {type: string}\n",
		"too deep":              "input:\n  a:\n    type: object\n    fields:\n      b:\n        type: object\n        fields:\n          c:\n            type: object\n            fields:\n              d:\n                type: object\n                fields:\n                  e: {type: string}\n",
	}
	for name, body := range bad {
		if _, err := ParseDecl([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.HasPrefix(err.Error(), "capability declaration:") {
			t.Errorf("%s: error should be prefixed: %v", name, err)
		}
	}
	good := map[string]string{
		"three levels":  "input:\n  a:\n    type: object\n    fields:\n      b:\n        type: object\n        fields:\n          c:\n            type: object\n            fields:\n              d: {type: string}\n",
		"enum untyped":  "input:\n  k:\n    enum: [a, b, 3]\n",
		"nullable":      "input:\n  k:\n    type: [integer, 'null']\n",
		"default false": "input:\n  k:\n    type: boolean\n    default: false\n",
	}
	for name, body := range good {
		if _, err := ParseDecl([]byte(body)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestInputSchemaDerivation(t *testing.T) {
	d, err := ParseDecl([]byte(typedDecl))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(d.InputSchema(), &s); err != nil {
		t.Fatal(err)
	}
	if s["type"] != "object" || s["additionalProperties"] != false {
		t.Errorf("root = %v", s)
	}
	req, _ := s["required"].([]any)
	if len(req) != 2 || req[0] != "maybe" || req[1] != "query" {
		t.Errorf("required = %v", s["required"])
	}
	props := s["properties"].(map[string]any)
	q := props["query"].(map[string]any)
	if q["type"] != "string" || q["maxLength"] != float64(500) || q["description"] != "a few words" {
		t.Errorf("query = %v", q)
	}
	if c := props["count"].(map[string]any); c["minimum"] != float64(1) || c["default"] != float64(10) {
		t.Errorf("count = %v", c)
	}
	if tg := props["tags"].(map[string]any); tg["items"].(map[string]any)["minLength"] != float64(1) {
		t.Errorf("tags = %v", tg)
	}
	o := props["options"].(map[string]any)
	if o["additionalProperties"] != false || o["required"].([]any)[0] != "depth" || o["properties"].(map[string]any)["verbose"].(map[string]any)["type"] != "boolean" {
		t.Errorf("options = %v", o)
	}
	p := props["part"].(map[string]any)
	if _, has := p["anyOf"]; !has || p["description"] != `which part, or "last"` {
		t.Errorf("part = %v", p)
	}
	if n := props["note"].(map[string]any); len(n) != 0 {
		t.Errorf("untyped note = %v, want {}", n)
	}
	if m := props["maybe"].(map[string]any); len(m["type"].([]any)) != 2 {
		t.Errorf("maybe = %v", m)
	}

	// The derived schema validates what the declaration means.
	check := func(doc string) []jsonschemax.Problem {
		var v any
		_ = json.Unmarshal([]byte(doc), &v)
		_, probs, err := jsonschemax.Validate(d.InputSchema(), v)
		if err != nil {
			t.Fatal(err)
		}
		return probs
	}
	if p := check(`{"query":"tides","maybe":null,"count":5,"part":"last","note":[1,2],"options":{"depth":2}}`); len(p) != 0 {
		t.Errorf("valid doc refused: %v", p)
	}
	if p := check(`{"query":"tides","maybe":"x","count":0,"part":"first","extra":1,"options":{}}`); len(p) < 4 {
		t.Errorf("invalid doc: want count, part, extra and options.depth problems, got %v", p)
	}

	// An empty or untyped declaration still derives a usable schema.
	e, _ := ParseDecl([]byte(""))
	if string(e.InputSchema()) != `{"additionalProperties":false,"properties":{},"required":[],"type":"object"}` {
		t.Errorf("empty schema = %s", e.InputSchema())
	}
	u, _ := ParseDecl([]byte("input:\n  command:\n    description: the whole command line\n    required: true\n"))
	if p := func() []jsonschemax.Problem {
		var v any
		_ = json.Unmarshal([]byte(`{"command": 42}`), &v)
		_, probs, _ := jsonschemax.Validate(u.InputSchema(), v)
		return probs
	}(); len(p) != 0 {
		t.Errorf("untyped field refused a value: %v", p)
	}
	// Output has the same derivation.
	o2, _ := ParseDecl([]byte("output:\n  mid:\n    type: string\n    required: true\n"))
	if !strings.Contains(string(o2.OutputSchema()), `"required":["mid"]`) {
		t.Errorf("output schema = %s", o2.OutputSchema())
	}
}

func TestToolNameLength(t *testing.T) {
	if ToolName("web.search") != "web_search" || ToolName("local.browser.open") != "local_browser_open" || ToolName("plain") != "plain" {
		t.Error("simple names")
	}
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60)
	tool := ToolName(long)
	if len(tool) != MaxToolName || tool != ToolName(long) || !strings.HasPrefix(tool, strings.Repeat("a", 55)+"_") {
		t.Errorf("long name = %q (%d)", tool, len(tool))
	}
	if ToolName(long) == ToolName(long+"c") {
		t.Error("two long names collided")
	}
}

func TestMissingNullableRequired(t *testing.T) {
	fields := map[string]Field{
		"plain":    {Required: true},
		"nullable": {Required: true, Type: TypeList{"string", "null"}},
		"opt":      {},
	}
	if got := Missing(fields, `{"plain":null,"nullable":null}`); len(got) != 1 || got[0] != "plain" {
		t.Errorf("Missing = %v, want [plain]", got)
	}
	if got := Missing(fields, `{"plain":"x"}`); len(got) != 1 || got[0] != "nullable" {
		t.Errorf("Missing = %v, want [nullable]", got)
	}
}
