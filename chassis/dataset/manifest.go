package dataset

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"

	yaml "go.yaml.in/yaml/v3"
)

// Manifest is the author-declared query surface of one dataset — the ONLY
// SQL the runtime will ever execute against the artifact. Parsed strictly:
// an unknown key is a deploy error, not a silent ignore, so a typo'd
// `querys:` can't ship a dataset with no queries.
//
//	queries:
//	  lookup_title:
//	    sql: |
//	      SELECT title, author FROM books_fts WHERE books_fts MATCH ? LIMIT 10
//	  lookup_isbn:
//	    sql: SELECT * FROM books WHERE isbn13 = ? LIMIT 1
//	    max_rows: 1
type Manifest struct {
	Queries map[string]Query `yaml:"queries"`
}

// Query is one named, parameterised, SELECT-only statement. MaxRows
// optionally tightens the node's DatasetMaxRows cap for this query; it can
// never widen it.
type Query struct {
	SQL     string `yaml:"sql"`
	MaxRows int    `yaml:"max_rows"`
}

// queryNameRe pins query names to txcl-friendly identifiers: they appear in
// WITH clauses and error payloads, so no whitespace/quoting surprises.
var queryNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ParseManifest strictly decodes + validates a DATASETS/<name>.yaml body.
func ParseManifest(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("dataset manifest: %w", err)
	}
	if len(m.Queries) == 0 {
		return nil, fmt.Errorf("dataset manifest: no queries declared (a dataset without queries is unreachable)")
	}
	for name, q := range m.Queries {
		if !queryNameRe.MatchString(name) {
			return nil, fmt.Errorf("dataset manifest: query name %q must match %s", name, queryNameRe)
		}
		if q.SQL == "" {
			return nil, fmt.Errorf("dataset manifest: query %q has empty sql", name)
		}
		if q.MaxRows < 0 {
			return nil, fmt.Errorf("dataset manifest: query %q has negative max_rows", name)
		}
	}
	return &m, nil
}

// QueryNames returns the declared names, sorted — for error hints
// ("unknown query X, have [a b c]") and deterministic validation order.
func (m *Manifest) QueryNames() []string {
	names := make([]string, 0, len(m.Queries))
	for n := range m.Queries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
