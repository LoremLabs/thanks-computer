package capdecl

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// The typed half of a field: how `input:` (and `output:`) say what shape a
// value has, with the keys JSON Schema uses, so a declaration can be handed
// to a model as a tool's parameters and a value can be checked against it.
// A field with none of these keys is untyped and accepts anything, which is
// what every declaration written before types existed means.

// MaxFieldDepth bounds how deep `fields` and `items` may nest.
const MaxFieldDepth = 3

// MaxToolName is the longest name a model-facing tool may have: what the
// providers accept.
const MaxToolName = 64

// typeNames are the JSON Schema types a field may name.
var typeNames = map[string]bool{"string": true, "integer": true, "number": true, "boolean": true, "array": true, "object": true, "null": true}

// TypeList is a field's `type`: one JSON Schema type name, or a list of
// them (`[string, "null"]` for a field that may be explicitly null). It
// reads a YAML scalar or sequence, and writes JSON as a string when it
// holds one name and as an array otherwise, which is what a schema wants.
type TypeList []string

// UnmarshalYAML reads a scalar or a sequence of scalars. It is the only
// custom YAML decoder in a declaration: yaml.v3 loses KnownFields inside
// one, so Field itself stays a plain struct.
func (t *TypeList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*t = TypeList{n.Value}
		return nil
	case yaml.SequenceNode:
		out := make(TypeList, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("type: a type name, or a list of type names")
			}
			out = append(out, c.Value)
		}
		*t = out
		return nil
	}
	return fmt.Errorf("type: a type name, or a list of type names")
}

// MarshalJSON writes one name as a string and several as an array.
func (t TypeList) MarshalJSON() ([]byte, error) {
	if len(t) == 1 {
		return json.Marshal(t[0])
	}
	return json.Marshal([]string(t))
}

// UnmarshalJSON reads what MarshalJSON writes.
func (t *TypeList) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*t = TypeList{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return fmt.Errorf("type: a type name, or a list of type names")
	}
	*t = many
	return nil
}

// Has reports whether the list names typ.
func (t TypeList) Has(typ string) bool {
	for _, x := range t {
		if x == typ {
			return true
		}
	}
	return false
}

// Typed reports whether the field says anything about its value's shape.
// An untyped field accepts anything.
func (f Field) Typed() bool {
	return len(f.Type) > 0 || f.Schema != nil || len(f.Enum) > 0
}

// checkField validates one field's typed keys, at `path` (for the error),
// `depth` levels down (0 at the top).
func checkField(kind, path string, f Field, depth int) error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%s: %s: %s", kind, path, fmt.Sprintf(format, args...))
	}
	if len(f.Description) > MaxInputDescription {
		return bad("description is longer than %d characters", MaxInputDescription)
	}
	if depth > MaxFieldDepth {
		return bad("fields nest deeper than %d levels", MaxFieldDepth)
	}
	for _, typ := range f.Type {
		if !typeNames[typ] {
			return bad("type %q is not one of string, integer, number, boolean, array, object, null", typ)
		}
	}
	if f.Schema != nil {
		if len(f.Type) > 0 || len(f.Enum) > 0 || f.Default != nil || f.Minimum != nil || f.Maximum != nil ||
			f.MinLength != nil || f.MaxLength != nil || f.Pattern != "" || f.Items != nil || f.Fields != nil {
			return bad("schema stands alone: it replaces type, enum, default, the bounds, items and fields")
		}
		if _, err := json.Marshal(f.Schema); err != nil {
			return bad("schema must be JSON (string keys): %v", err)
		}
		return nil
	}
	hasType := len(f.Type) > 0
	numeric := f.Type.Has("number") || f.Type.Has("integer")
	if (f.Minimum != nil || f.Maximum != nil) && !numeric {
		return bad("minimum and maximum need type number or integer")
	}
	if f.Minimum != nil && f.Maximum != nil && *f.Minimum > *f.Maximum {
		return bad("minimum is above maximum")
	}
	if (f.MinLength != nil || f.MaxLength != nil || f.Pattern != "") && !f.Type.Has("string") {
		return bad("min_length, max_length and pattern need type string")
	}
	if (f.MinLength != nil && *f.MinLength < 0) || (f.MaxLength != nil && *f.MaxLength < 0) {
		return bad("min_length and max_length are 0 or more")
	}
	if f.MinLength != nil && f.MaxLength != nil && *f.MinLength > *f.MaxLength {
		return bad("min_length is above max_length")
	}
	if f.Pattern != "" {
		if _, err := regexp.Compile(f.Pattern); err != nil {
			return bad("pattern does not compile (RE2 syntax): %v", err)
		}
	}
	if f.Items != nil {
		if !f.Type.Has("array") {
			return bad("items needs type array")
		}
		if err := checkField(kind, path+".items", *f.Items, depth+1); err != nil {
			return err
		}
	}
	if f.Fields != nil {
		if !f.Type.Has("object") {
			return bad("fields needs type object")
		}
		if len(f.Fields) > MaxInputs {
			return bad("fields names %d fields, at most %d", len(f.Fields), MaxInputs)
		}
		names := make([]string, 0, len(f.Fields))
		for n := range f.Fields {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if !ValidInput(n) {
				return bad("fields.%s: a field is a lowercase letter, then lowercase letters, digits and _ (1-64 chars)", n)
			}
			if err := checkField(kind, path+".fields."+n, f.Fields[n], depth+1); err != nil {
				return err
			}
		}
	}
	if len(f.Enum) > 0 {
		if _, err := json.Marshal(f.Enum); err != nil {
			return bad("enum must be JSON values: %v", err)
		}
	}
	if f.Default != nil {
		if _, err := json.Marshal(f.Default); err != nil {
			return bad("default must be a JSON value: %v", err)
		}
	}
	_ = hasType
	return nil
}

// schema is the field as a JSON Schema: the typed keys under their schema
// names, nested fields as properties with their required list, and
// nothing but a description for an untyped field. `schema:` is spliced in
// as written, with the description added when it has none.
func (f Field) schema() map[string]any {
	if f.Schema != nil {
		out := make(map[string]any, len(f.Schema)+1)
		for k, v := range f.Schema {
			out[k] = v
		}
		if f.Description != "" {
			if _, has := out["description"]; !has {
				out["description"] = f.Description
			}
		}
		return out
	}
	out := map[string]any{}
	if f.Description != "" {
		out["description"] = f.Description
	}
	if len(f.Type) > 0 {
		if len(f.Type) == 1 {
			out["type"] = f.Type[0]
		} else {
			out["type"] = []string(f.Type)
		}
	}
	if len(f.Enum) > 0 {
		out["enum"] = f.Enum
	}
	if f.Default != nil {
		out["default"] = f.Default
	}
	if f.Minimum != nil {
		out["minimum"] = *f.Minimum
	}
	if f.Maximum != nil {
		out["maximum"] = *f.Maximum
	}
	if f.MinLength != nil {
		out["minLength"] = *f.MinLength
	}
	if f.MaxLength != nil {
		out["maxLength"] = *f.MaxLength
	}
	if f.Pattern != "" {
		out["pattern"] = f.Pattern
	}
	if f.Items != nil {
		out["items"] = f.Items.schema()
	}
	if f.Fields != nil {
		out["properties"], out["required"] = propertiesOf(f.Fields)
		out["additionalProperties"] = false
	}
	return out
}

// propertiesOf renders a field map as a schema's properties and its sorted
// required list (always present, possibly empty, so the shape is stable).
func propertiesOf(fields map[string]Field) (map[string]any, []string) {
	props := make(map[string]any, len(fields))
	required := []string{}
	for n, f := range fields {
		props[n] = f.schema()
		if f.Required {
			required = append(required, n)
		}
	}
	sort.Strings(required)
	return props, required
}

// objectSchema is the schema of a whole input or output: an object whose
// properties are the fields, closed to unknown keys.
func objectSchema(fields map[string]Field) json.RawMessage {
	props, required := propertiesOf(fields)
	b, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	})
	return b
}

// InputSchema is the declaration's input as a JSON Schema object: one
// property per field, the required ones listed, unknown keys refused. An
// untyped field is `{description}` and accepts any value, so a declaration
// written before types existed yields a valid schema with no edit. The
// catalogue hands this to a model as a tool's parameters.
func (d *Decl) InputSchema() json.RawMessage {
	if d.inputSchema == nil {
		d.inputSchema = objectSchema(d.Input)
	}
	return d.inputSchema
}

// OutputSchema is the declaration's output in the same shape.
func (d *Decl) OutputSchema() json.RawMessage {
	if d.outputSchema == nil {
		d.outputSchema = objectSchema(d.Output)
	}
	return d.outputSchema
}

// ToolName is the model-facing name of the capability called name: the
// capability's name with every `.` as `_`, which every provider accepts.
// A name that would pass 64 characters keeps its first 55, then `_` and 8
// hex digits of its SHA-256, so it stays deterministic. Two capabilities
// can derive one tool name (`web.search` and `web_search`); lint,
// activation and the catalogue refuse that.
func ToolName(name string) string {
	tool := strings.ReplaceAll(name, ".", "_")
	if len(tool) <= MaxToolName {
		return tool
	}
	sum := sha256.Sum256([]byte(name))
	return tool[:MaxToolName-9] + "_" + hex.EncodeToString(sum[:])[:8]
}
