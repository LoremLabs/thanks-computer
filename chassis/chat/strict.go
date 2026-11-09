package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Strict decoding: a provider's constrained sampling, under which a model
// can only produce arguments (or an answer) that match the schema. OpenAI's
// grammar accepts a subset of JSON Schema and demands that every property
// be required, so an optional field is sent as required-and-nullable and a
// null the model returns for it is removed again on the way back. This file
// is the provider-neutral half: which schemas qualify, the transform, and
// the undo. A dialect decides whether and how to send it.
//
// The transform records every field it made nullable as a JSON Pointer into
// the instance (`/options/limit`; `/items/-/label` for the elements of an
// array), so the undo removes a null only where the chassis put the
// nullability, never where a declaration allowed null itself.

// ErrNotStrict is wrapped by Qualifies's reason when a schema cannot be
// decoded strictly.
var ErrNotStrict = errors.New("schema does not qualify for strict decoding")

// Qualifies reports whether schema can be sent for strict decoding: an
// object schema whose every property is typed, with no open
// additionalProperties, no optional property that already allows null (its
// absence could not be told from a null), no references and no keywords
// the strict grammar refuses. The reason says which rule failed.
func Qualifies(schema json.RawMessage) (ok bool, reason string) {
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil || root == nil {
		return false, "not a JSON object"
	}
	if err := checkStrictNode(root, "", true); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// unsupportedStrictKeys are JSON Schema keywords the strict grammar refuses.
var unsupportedStrictKeys = []string{"$ref", "$defs", "definitions", "oneOf", "allOf", "not", "if", "then", "else", "patternProperties", "dependentSchemas", "unevaluatedProperties", "contains", "prefixItems", "const"}

func checkStrictNode(node map[string]any, at string, root bool) error {
	where := func() string {
		if at == "" {
			return "the root"
		}
		return at
	}
	for _, k := range unsupportedStrictKeys {
		if _, has := node[k]; has {
			return fmt.Errorf("%s: %q is not allowed under strict decoding", where(), k)
		}
	}
	typ, _ := node["type"].(string)
	types, _ := node["type"].([]any)
	if typ == "" && len(types) == 0 {
		if _, isAnyOf := node["anyOf"]; !isAnyOf {
			return fmt.Errorf("%s: every field needs a type under strict decoding", where())
		}
	}
	if root && typ != "object" {
		return errors.New("the root must be an object schema")
	}
	if anyOf, has := node["anyOf"].([]any); has {
		for i, v := range anyOf {
			sub, ok := v.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: anyOf[%d] is not a schema", where(), i)
			}
			if st, _ := sub["type"].(string); st == "object" || st == "array" {
				return fmt.Errorf("%s: anyOf of objects or arrays is not allowed under strict decoding", where())
			}
			if err := checkStrictNode(sub, fmt.Sprintf("%s/anyOf/%d", at, i), false); err != nil {
				return err
			}
		}
	}
	if items, has := node["items"]; has {
		sub, ok := items.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: items must be one schema", where())
		}
		if err := checkStrictNode(sub, at+"/-", false); err != nil {
			return err
		}
	}
	if typ == "object" || hasType(types, "object") {
		if ap, has := node["additionalProperties"]; has && ap != false {
			return fmt.Errorf("%s: additionalProperties must be false under strict decoding", where())
		}
		props, _ := node["properties"].(map[string]any)
		required := map[string]bool{}
		if req, ok := node["required"].([]any); ok {
			for _, r := range req {
				if s, ok := r.(string); ok {
					required[s] = true
				}
			}
		}
		names := make([]string, 0, len(props))
		for n := range props {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			sub, ok := props[n].(map[string]any)
			if !ok {
				return fmt.Errorf("%s: property %q is not a schema", where(), n)
			}
			if !required[n] && allowsNull(sub) {
				return fmt.Errorf("%s: optional property %q allows null, so its absence could not be told from a null under strict decoding", where(), n)
			}
			if err := checkStrictNode(sub, at+"/"+escapePointer(n), false); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasType(types []any, want string) bool {
	for _, t := range types {
		if s, _ := t.(string); s == want {
			return true
		}
	}
	return false
}

// allowsNull reports whether a schema admits null as a value.
func allowsNull(node map[string]any) bool {
	if s, _ := node["type"].(string); s == "null" {
		return true
	}
	if types, ok := node["type"].([]any); ok && hasType(types, "null") {
		return true
	}
	if enum, ok := node["enum"].([]any); ok {
		for _, e := range enum {
			if e == nil {
				return true
			}
		}
	}
	if anyOf, ok := node["anyOf"].([]any); ok {
		for _, v := range anyOf {
			if sub, ok := v.(map[string]any); ok && allowsNull(sub) {
				return true
			}
		}
	}
	return false
}

// MakeStrict rewrites a qualifying schema into the strict form: every
// property required, each optional one wrapped as `anyOf: [<schema>,
// {type: null}]`, `additionalProperties: false` on every object, applied
// through nested objects and array items. `default` and `description`
// stay as annotations. wrapped lists the instance pointers of the fields
// made nullable, for StripWrappedNulls.
func MakeStrict(schema json.RawMessage) (out json.RawMessage, wrapped []string, err error) {
	if ok, reason := Qualifies(schema); !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotStrict, reason)
	}
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, nil, err
	}
	wrapped = []string{}
	strictNode(root, "", &wrapped)
	sort.Strings(wrapped)
	b, err := json.Marshal(root)
	if err != nil {
		return nil, nil, err
	}
	return b, wrapped, nil
}

func strictNode(node map[string]any, at string, wrapped *[]string) {
	if anyOf, ok := node["anyOf"].([]any); ok {
		for _, v := range anyOf {
			if sub, ok := v.(map[string]any); ok {
				strictNode(sub, at, wrapped)
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		strictNode(items, at+"/-", wrapped)
	}
	typ, _ := node["type"].(string)
	types, _ := node["type"].([]any)
	if typ != "object" && !hasType(types, "object") {
		return
	}
	props, _ := node["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		node["properties"] = props
	}
	required := map[string]bool{}
	if req, ok := node["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sub := props[n].(map[string]any)
		strictNode(sub, at+"/"+escapePointer(n), wrapped)
		if !required[n] {
			props[n] = map[string]any{"anyOf": []any{sub, map[string]any{"type": "null"}}}
			*wrapped = append(*wrapped, at+"/"+escapePointer(n))
		}
	}
	all := make([]any, 0, len(names))
	for _, n := range names {
		all = append(all, n)
	}
	node["required"] = all
	node["additionalProperties"] = false
}

// StripWrappedNulls removes, from doc, a null at each wrapped pointer, and
// nothing else: a null a declaration allowed stays. A pointer segment `-`
// means every element of the array there. doc comes back unchanged when it
// is not valid JSON.
func StripWrappedNulls(doc json.RawMessage, wrapped []string) json.RawMessage {
	if len(wrapped) == 0 || len(doc) == 0 {
		return doc
	}
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return doc
	}
	for _, p := range wrapped {
		segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
		for i := range segs {
			segs[i] = unescapePointer(segs[i])
		}
		stripAt(v, segs)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return doc
	}
	return b
}

// stripAt walks segs into v and deletes the last key when its value is
// null; `-` fans out over an array's elements.
func stripAt(v any, segs []string) {
	if len(segs) == 0 {
		return
	}
	switch node := v.(type) {
	case map[string]any:
		if segs[0] == "-" {
			return
		}
		child, has := node[segs[0]]
		if !has {
			return
		}
		if len(segs) == 1 {
			if child == nil {
				delete(node, segs[0])
			}
			return
		}
		stripAt(child, segs[1:])
	case []any:
		if segs[0] != "-" {
			return
		}
		for _, el := range node {
			stripAt(el, segs[1:])
		}
	}
}

func escapePointer(s string) string {
	return strings.NewReplacer("~", "~0", "/", "~1").Replace(s)
}

func unescapePointer(s string) string {
	return strings.NewReplacer("~1", "/", "~0", "~").Replace(s)
}
