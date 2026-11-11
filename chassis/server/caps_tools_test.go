package server

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
)

// fakeCaps is a catalogue of declarations written in the test.
type fakeCaps map[string]string // name → declaration body; stack is "s"

func (f fakeCaps) List(context.Context, string) ([]capEntry, error) {
	var out []capEntry
	for name, body := range f {
		d, err := capdecl.ParseDecl([]byte(body))
		out = append(out, capEntry{Name: name, Stack: "s", Decl: d, Err: err})
	}
	return out, nil
}

func TestCapsListParametersAndNames(t *testing.T) {
	d := capsDeps{decls: capDeclFixture(t)}
	out := callCaps(t, d, "acme", "loop", `{"names":["mail.send","nope.here"]}`)
	caps := gjson.Get(out, "_caps")
	if caps.Get("count").Int() != 1 || caps.Get("items.0.name").String() != "mail.send" {
		t.Fatalf("names filter: %s", out)
	}
	if caps.Get("missing").Raw != `["nope.here"]` {
		t.Errorf("missing = %s", caps.Get("missing").Raw)
	}
	// Every item carries its derived input schema; an untyped required field
	// is a property that accepts anything and is listed as required.
	params := caps.Get("items.0.parameters")
	if params.Get("type").String() != "object" || params.Get("required").Raw != `["to"]` || params.Get("additionalProperties").Bool() != false {
		t.Errorf("parameters = %s", params.Raw)
	}
	if !params.Get("properties.to").Exists() {
		t.Errorf("parameters.properties = %s", params.Get("properties").Raw)
	}
	// names and prefix compose; a bad names value is an in-band error.
	out = callCaps(t, d, "acme", "loop", `{"names":["mail.send"],"prefix":"local."}`)
	if gjson.Get(out, "_caps.count").Int() != 0 || gjson.Get(out, "_caps.missing").Raw != `["mail.send"]` {
		t.Errorf("names+prefix: %s", out)
	}
	out = callCaps(t, d, "acme", "loop", `{"names":"mail.send"}`)
	if gjson.Get(out, "_caps.error.code").String() != "txco_caps_invalid_arg" {
		t.Errorf("bad names: %s", out)
	}
	out = callCaps(t, d, "acme", "loop", `{"format":"xml"}`)
	if gjson.Get(out, "_caps.error.code").String() != "txco_caps_invalid_arg" {
		t.Errorf("bad format: %s", out)
	}
}

func TestCapsListFormatTools(t *testing.T) {
	d := capsDeps{decls: fakeCaps{
		"web.search": "description: Search the web.\neffect: read\ninput:\n  query:\n    type: string\n    required: true\n  count:\n    type: integer\n    minimum: 1\n    default: 10\n",
		"docs.read":  "description: Read a document.\ninput:\n  name: {type: string}\n",
		"run.finish": "description: Finish.\neffect: write\n",
		"broken":     "entry: []\n",
	}}
	out := callCaps(t, d, "acme", "loop", `{"format":"tools","into":"_tools"}`)
	tools := gjson.Get(out, "_tools")
	if tools.Get("count").Int() != 3 {
		t.Fatalf("tools = %s", out)
	}
	// Sorted by tool name, dots as underscores, the capability in the binding.
	if names := tools.Get("tools.#.name").Raw; names != `["docs_read","run_finish","web_search"]` {
		t.Errorf("names = %s", names)
	}
	ws := tools.Get(`tools.#(name=="web_search")`)
	if ws.Get("binding.capability").String() != "web.search" || ws.Get("description").String() != "Search the web." {
		t.Errorf("web_search = %s", ws.Raw)
	}
	if ws.Get("parameters.properties.query.type").String() != "string" || ws.Get("parameters.required").Raw != `["query"]` || ws.Get("parameters.properties.count.default").Int() != 10 {
		t.Errorf("web_search parameters = %s", ws.Get("parameters").Raw)
	}
	// A capability with no input has an empty object schema, still closed.
	rf := tools.Get(`tools.#(name=="run_finish")`)
	if rf.Get("parameters.type").String() != "object" || rf.Get("parameters.additionalProperties").Bool() != false {
		t.Errorf("run_finish parameters = %s", rf.Get("parameters").Raw)
	}
	// The declaration's effect rides beside the binding, for the executor:
	// read or write as declared, and ABSENT when the declaration says
	// nothing (never a default the declaration did not write).
	if ws.Get("effect").String() != "read" || rf.Get("effect").String() != "write" {
		t.Errorf("effects: web_search %s, run_finish %s", ws.Get("effect").Raw, rf.Get("effect").Raw)
	}
	if dr := tools.Get(`tools.#(name=="docs_read")`); dr.Get("effect").Exists() {
		t.Errorf("a declaration with no effect reports one: %s", dr.Raw)
	}
	// Nothing a tool definition does not need: no stack, entry or stage.
	if ws.Get("stack").Exists() || ws.Get("stage").Exists() || ws.Get("input").Exists() {
		t.Errorf("tool carries catalogue fields: %s", ws.Raw)
	}
	if tools.Get("broken.0.name").String() != "broken" {
		t.Errorf("broken = %s", tools.Get("broken").Raw)
	}
	// names narrows the tool list too, and reports what was not found.
	out = callCaps(t, d, "acme", "loop", `{"format":"tools","names":["web.search","mail.send"]}`)
	if gjson.Get(out, "_caps.count").Int() != 1 || gjson.Get(out, "_caps.missing").Raw != `["mail.send"]` {
		t.Errorf("tools with names: %s", out)
	}
	// The catalogue's own listing says the effect too, and is silent where the declaration is.
	items := gjson.Get(callCaps(t, d, "acme", "loop", `{}`), "_caps.items")
	if items.Get(`#(name=="web.search").effect`).String() != "read" || items.Get(`#(name=="run.finish").effect`).String() != "write" ||
		items.Get(`#(name=="docs.read").effect`).Exists() {
		t.Errorf("the listing's effects: %s", items.Get("#.effect").Raw)
	}
}

func TestCapsListToolNameCollision(t *testing.T) {
	d := capsDeps{decls: fakeCaps{
		"web.search": "description: a\n",
		"web_search": "description: b\n",
	}}
	out := callCaps(t, d, "acme", "loop", `{"format":"tools"}`)
	if gjson.Get(out, "_caps.error.code").String() != "txco_caps_tool_names" {
		t.Errorf("collision not refused: %s", out)
	}
	// The plain list is unaffected: both capabilities exist.
	out = callCaps(t, d, "acme", "loop", `{}`)
	if gjson.Get(out, "_caps.count").Int() != 2 {
		t.Errorf("plain list: %s", out)
	}
}
