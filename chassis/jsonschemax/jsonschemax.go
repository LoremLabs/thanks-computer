// Package jsonschemax is the one place the chassis compiles a JSON Schema
// that came from a tenant: a rule's `WITH schema`, a capability's typed
// input, `txco://schema/check`. It pins the draft, bounds the size, and
// refuses every reference the schema cannot satisfy from itself.
//
// The last point is the reason the package exists. santhosh-tekuri's
// compiler resolves an unknown `$ref` through a global loader registry whose
// `file` entry opens the path on this host, so an untrusted schema with
// `$ref: "file:///etc/hosts"` would read the chassis's disk. Compile sets the
// compiler's LoadURL to a function that refuses, so the only resource a
// schema can reach is itself.
package jsonschemax

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// MaxSchemaBytes bounds one schema. A tool's parameters or an answer schema
// is a few hundred bytes to a few kilobytes; a quarter megabyte is already
// far past anything a model should be handed.
const MaxSchemaBytes = 256 << 10

// resourceURL is the name the schema is registered under. Nothing else is
// ever registered, so a `$ref` to any other document fails to resolve.
const resourceURL = "inline://schema"

// ErrRemoteRef is wrapped by the error a compile returns when the schema
// refers to a document outside itself.
var ErrRemoteRef = errors.New("a $ref to another document is refused")

// ErrEmpty is returned for an empty schema.
var ErrEmpty = errors.New("empty schema")

// ErrTooLarge is wrapped by the error a compile returns for a schema past
// MaxSchemaBytes.
var ErrTooLarge = errors.New("schema is too large")

// Compile parses and compiles raw as a JSON Schema (draft 2020-12 when the
// schema names no `$schema`). Every `$ref` must point inside the schema
// itself: a reference to a file or a URL is refused with ErrRemoteRef.
func Compile(raw []byte) (*jsonschema.Schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, ErrEmpty
	}
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("%w: %d bytes, at most %d", ErrTooLarge, len(raw), MaxSchemaBytes)
	}
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	c.LoadURL = func(s string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("%w: %s", ErrRemoteRef, s)
	}
	if err := c.AddResource(resourceURL, bytes.NewReader(raw)); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	sch, err := c.Compile(resourceURL)
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return sch, nil
}

// Problem is one place a value fails its schema: a JSON Pointer into the
// value ("" is the value itself) and the validator's sentence.
type Problem struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// Check validates value (a decoded JSON document) against sch and returns
// the leaf problems: the innermost failures, each with its own pointer, and
// not the wrapper nodes that only say a parent "doesn't validate". A nil
// result means the value is valid.
func Check(sch *jsonschema.Schema, value any) []Problem {
	err := sch.Validate(value)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Problem{{Message: err.Error()}}
	}
	var out []Problem
	var walk func(*jsonschema.ValidationError)
	walk = func(v *jsonschema.ValidationError) {
		if len(v.Causes) == 0 {
			out = append(out, Problem{Path: v.InstanceLocation, Message: v.Message})
			return
		}
		for _, c := range v.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// Validate compiles raw and checks value against it in one call. ok is false
// with the problems listed when the value fails; err is set only when the
// schema itself cannot be compiled.
func Validate(raw []byte, value any) (ok bool, problems []Problem, err error) {
	sch, err := Compile(raw)
	if err != nil {
		return false, nil, err
	}
	problems = Check(sch, value)
	return len(problems) == 0, problems, nil
}
