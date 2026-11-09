package server

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
)

func callSchemaCheck(t *testing.T, metaJSON string) string {
	t.Helper()
	ctx := operation.WithMeta(context.Background(), metaJSON)
	pl, err := schemaCheck(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

const toolParams = `{"type":"object","properties":{"q":{"type":"string","minLength":1},"count":{"type":"integer","minimum":1,"maximum":20}},"required":["q"],"additionalProperties":false}`

func TestSchemaCheckOK(t *testing.T) {
	out := callSchemaCheck(t, `{"schema":`+toolParams+`,"value":{"q":"tides","count":3}}`)
	if !gjson.Get(out, "_schema.ok").Bool() || gjson.Get(out, "_schema.errors").Exists() {
		t.Errorf("out = %s", out)
	}
	// The schema may arrive as a string (read from a file, say); into moves the answer.
	out = callSchemaCheck(t, `{"schema":"`+jsonString(toolParams)+`","value":{"q":"x"},"into":"_args"}`)
	if !gjson.Get(out, "_args.ok").Bool() || gjson.Get(out, "_schema").Exists() {
		t.Errorf("string schema / into: %s", out)
	}
}

func TestSchemaCheckErrorsWithPaths(t *testing.T) {
	out := callSchemaCheck(t, `{"schema":`+toolParams+`,"value":{"q":"","count":50,"extra":true}}`)
	if gjson.Get(out, "_schema.ok").Bool() {
		t.Fatalf("invalid value passed: %s", out)
	}
	errs := gjson.Get(out, "_schema.errors").Array()
	if len(errs) < 3 {
		t.Fatalf("errors = %s", gjson.Get(out, "_schema.errors").Raw)
	}
	paths := map[string]bool{}
	for _, e := range errs {
		paths[e.Get("path").String()] = true
		if e.Get("message").String() == "" {
			t.Errorf("error without message: %s", e.Raw)
		}
	}
	if !paths["/q"] || !paths["/count"] || !paths[""] {
		t.Errorf("paths = %v, want /q, /count and the root (the extra key)", paths)
	}
	// null is a value and is checked; a non-object value against an object schema fails at the root.
	out = callSchemaCheck(t, `{"schema":`+toolParams+`,"value":null}`)
	if gjson.Get(out, "_schema.ok").Bool() || gjson.Get(out, "_schema.errors.0.path").String() != "" {
		t.Errorf("null value: %s", out)
	}
}

func TestSchemaCheckAuthoringErrors(t *testing.T) {
	cases := map[string]string{
		"no schema":       `{"value":{}}`,
		"schema array":    `{"schema":[1],"value":{}}`,
		"schema number":   `{"schema":5,"value":{}}`,
		"no value":        `{"schema":` + toolParams + `}`,
		"bad schema":      `{"schema":{"type":"nope"},"value":{}}`,
		"remote ref":      `{"schema":{"type":"object","properties":{"x":{"$ref":"file:///etc/hosts"}}},"value":{"x":1}}`,
		"schema not json": `{"schema":"{not json","value":{}}`,
	}
	for name, meta := range cases {
		t.Run(name, func(t *testing.T) {
			out := callSchemaCheck(t, meta)
			code := gjson.Get(out, "_schema.error.code").String()
			if code != "txco_schema_invalid_with" && code != "txco_schema_invalid" {
				t.Errorf("code = %q (out %s)", code, out)
			}
			if gjson.Get(out, "_schema.ok").Exists() {
				t.Errorf("ok written beside an error: %s", out)
			}
		})
	}
}

// jsonString escapes s for use inside a JSON string literal.
func jsonString(s string) string {
	out := make([]byte, 0, len(s)+8)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			out = append(out, '\\', c)
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
