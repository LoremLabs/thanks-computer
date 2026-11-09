package server

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonschemax"
	"github.com/loremlabs/thanks-computer/chassis/operation"
)

// txco://schema/check — a value against a JSON Schema, in one op.
//
//	WHEN ._turn.tool_calls.0.name =~ /./
//	  WITH schema = ._tools.tools.0.parameters,
//	       value  = ._turn.tool_calls.0.arguments,
//	       into   = "_check"
//	  EXEC "txco://schema/check"
//	# → _check = {ok: true} | {ok: false, errors: [{path, message}, …]}
//
// What an executor runs before a `cap://` call a model asked for: the
// schema is the tool's parameters (the catalogue's, an MCP server's, an
// OpenAPI operation's), the value is the model's arguments, and the
// executor is the one that knows where the schema came from, so the check
// is its and not the model adapter's. The same op checks anything else a
// stack holds a schema for.
//
// The schema is compiled as every tenant schema is (chassis/jsonschemax):
// draft 2020-12, at most 256 KiB, and a `$ref` may point only inside it.
// A schema that does not compile, or a missing value, is the author's
// mistake and comes back in band as `<into>.error`, so the run goes on.

const schemaCheckDefaultInto = "_schema"

type schemaCheckOut struct {
	OK     bool                  `json:"ok"`
	Errors []jsonschemax.Problem `json:"errors,omitempty"`
}

func schemaCheck(ctx context.Context, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, schemaCheckDefaultInto)

	schemaRaw := gjson.GetBytes(meta, "schema")
	if !schemaRaw.Exists() {
		return identityErr(into, "schema", "invalid_with", "WITH schema is required: a JSON Schema object, or a string holding one"), nil
	}
	var schema []byte
	switch schemaRaw.Type {
	case gjson.JSON:
		if !strings.HasPrefix(strings.TrimSpace(schemaRaw.Raw), "{") {
			return identityErr(into, "schema", "invalid_with", "WITH schema must be a JSON Schema object"), nil
		}
		schema = []byte(schemaRaw.Raw)
	case gjson.String:
		schema = []byte(schemaRaw.String())
	default:
		return identityErr(into, "schema", "invalid_with", "WITH schema must be a JSON Schema object"), nil
	}

	valueRaw := gjson.GetBytes(meta, "value")
	if !valueRaw.Exists() {
		return identityErr(into, "schema", "invalid_with", "WITH value is required: the JSON value to check (null is a value; an absent key is not)"), nil
	}
	var value any
	if err := json.Unmarshal([]byte(valueRaw.Raw), &value); err != nil {
		return identityErr(into, "schema", "invalid_with", "WITH value is not JSON: "+err.Error()), nil
	}

	ok, problems, err := jsonschemax.Validate(schema, value)
	if err != nil {
		return identityErr(into, "schema", "invalid", "the schema does not compile: "+err.Error()), nil
	}
	return identityOK(into, schemaCheckOut{OK: ok, Errors: problems}), nil
}
