// Package webabi is the TxCo Web ABI on the installing side: the manifest
// (txco-web.json), a loaded ABI directory, the marker and provenance files
// an install writes, and the collision rules for laying an ABI directory
// over an author's stack tree. It has no CLI dependencies; `txco apply`,
// `txco push`, `txco dev` and `txco web check` build on it.
//
// An ABI directory is a producer's build output:
//
//	txco-web.json   the manifest: what the artifact is, never its routing
//	public/         files, installed as the stack's FILES/
//	server/         an optional Fetch handler module (refused until a runner exists)
//	ops/            ordinary .txcl in scope directories (ops/900000/…)
package webabi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"

	webabisdk "github.com/loremlabs/thanks-computer/sdk/web-abi"
)

const (
	// ManifestName is the manifest's file name at the root of an ABI directory.
	ManifestName = "txco-web.json"
	// ABIVersion is the manifest's abi value this package reads.
	ABIVersion = 1
	// ProducerScope starts the scope band producers' ops live in, after
	// every op an author writes.
	ProducerScope = 900000
)

// Manifest is txco-web.json.
type Manifest struct {
	ABI       int      `json:"abi"`
	Server    *Server  `json:"server,omitempty"`
	Immutable []string `json:"immutable,omitempty"`
}

// Server names the application server's entry module.
type Server struct {
	Entry string `json:"entry"`
}

// Problem is one thing wrong with a manifest, at a JSON pointer.
type Problem struct {
	Pointer string
	Message string
}

// ManifestError lists everything wrong with a manifest.
type ManifestError struct {
	Problems []Problem
}

func (e *ManifestError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		ptr := p.Pointer
		if ptr == "" {
			ptr = "/"
		}
		parts = append(parts, ptr+": "+p.Message)
	}
	return ManifestName + ": " + strings.Join(parts, "; ")
}

var (
	schemaOnce sync.Once
	schema     *jsonschema.Schema
	schemaErr  error
)

func compiledSchema() (*jsonschema.Schema, error) {
	schemaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.Draft = jsonschema.Draft2020
		const url = "https://thanks.computer/schemas/txco-web-1.json"
		if err := c.AddResource(url, bytes.NewReader(webabisdk.ManifestSchema)); err != nil {
			schemaErr = err
			return
		}
		schema, schemaErr = c.Compile(url)
	})
	return schema, schemaErr
}

// ParseManifest validates data against the manifest schema, then the rules
// a schema can't express, and decodes it.
func ParseManifest(data []byte) (*Manifest, error) {
	sch, err := compiledSchema()
	if err != nil {
		return nil, fmt.Errorf("manifest schema: %w", err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, &ManifestError{Problems: []Problem{{Message: "not valid JSON: " + err.Error()}}}
	}
	if err := sch.Validate(doc); err != nil {
		ve, ok := err.(*jsonschema.ValidationError)
		if !ok {
			return nil, err
		}
		var probs []Problem
		for _, e := range ve.BasicOutput().Errors {
			// The root entry only says "doesn't validate"; its causes say why.
			if e.KeywordLocation == "" {
				continue
			}
			probs = append(probs, Problem{Pointer: e.InstanceLocation, Message: e.Error})
		}
		if len(probs) == 0 {
			probs = []Problem{{Message: ve.Error()}}
		}
		return nil, &ManifestError{Problems: probs}
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, &ManifestError{Problems: []Problem{{Message: err.Error()}}}
	}

	var probs []Problem
	for i, p := range m.Immutable {
		trimmed := strings.TrimSuffix(p, "/")
		ptr := fmt.Sprintf("/immutable/%d", i)
		switch {
		case path.Clean(trimmed) != trimmed || hasDotSegment(trimmed):
			probs = append(probs, Problem{ptr, "a prefix must be a clean path, with no '.' or '..' segment"})
		case firstSegment(trimmed) == "_txco":
			probs = append(probs, Problem{ptr, "_txco/ is reserved for the installer"})
		}
	}
	if m.Server != nil {
		e := m.Server.Entry
		if path.Clean(e) != e || hasDotSegment(e) || !strings.HasPrefix(e, "server/") {
			probs = append(probs, Problem{"/server/entry", "the entry must be a clean path under server/"})
		}
	}
	if len(probs) > 0 {
		return nil, &ManifestError{Problems: probs}
	}
	return &m, nil
}

// ReadManifest reads and parses <dir>/txco-web.json.
func ReadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s: no %s — is this a Web ABI directory (a producer's build output)?", dir, ManifestName)
		}
		return nil, err
	}
	return ParseManifest(data)
}

// ImmutablePrefixes returns the manifest's immutable prefixes, each ending
// in "/", sorted.
func (m *Manifest) ImmutablePrefixes() []string {
	out := make([]string, 0, len(m.Immutable))
	for _, p := range m.Immutable {
		out = append(out, strings.TrimSuffix(p, "/")+"/")
	}
	sort.Strings(out)
	return out
}

func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func firstSegment(p string) string {
	seg, _, _ := strings.Cut(p, "/")
	return seg
}
