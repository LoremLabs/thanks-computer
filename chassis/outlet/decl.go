package outlet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"

	yaml "go.yaml.in/yaml/v3"
)

// Access levels. `read` refuses outlet://<name>/exec before it reaches the
// driver; `write` allows it. The database role behind the DSN remains the
// real boundary for what a write may touch.
const (
	AccessRead  = "read"
	AccessWrite = "write"
)

// Decl is one OUTLETS/<name>.yaml: where to connect and the ceilings. The
// SQL lives in the ops that call the outlet, not here.
//
//	driver: postgres
//	secret: CRM_DSN      # a secret NAME; its value is the DSN
//	access: write        # read (default) | write
//	max_rows: 500        # tightens the node ceiling, never raises it
//	timeout: 5000        # ms; caps every call on this outlet
//
// Parsed strictly: an unknown key is a deploy error, not a silent ignore.
type Decl struct {
	Driver  string `yaml:"driver"`
	Secret  string `yaml:"secret"`
	Access  string `yaml:"access"`
	MaxRows int    `yaml:"max_rows"`
	Timeout int    `yaml:"timeout"`
}

// secretNameRe mirrors the secret store's name rule (chassis/secrets:
// ErrInvalidName) so a declaration can't reference a name the store could
// never hold.
var secretNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// ParseDecl strictly decodes and validates a declaration body. knownDriver
// reports whether a driver name is built into this binary; nil skips that
// check (the runtime looks the driver up again when it opens the pool).
func ParseDecl(data []byte, knownDriver func(string) bool) (*Decl, error) {
	var d Decl
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("outlet declaration: %w", err)
	}
	if d.Driver == "" {
		return nil, fmt.Errorf("outlet declaration: driver is required")
	}
	if knownDriver != nil && !knownDriver(d.Driver) {
		return nil, fmt.Errorf("outlet declaration: unknown driver %q (available: %v)", d.Driver, Registered())
	}
	if d.Secret == "" {
		return nil, fmt.Errorf("outlet declaration: secret is required (the NAME of the secret holding the DSN)")
	}
	if !secretNameRe.MatchString(d.Secret) {
		return nil, fmt.Errorf("outlet declaration: secret %q must match %s", d.Secret, secretNameRe)
	}
	switch d.Access {
	case "":
		d.Access = AccessRead
	case AccessRead, AccessWrite:
	default:
		return nil, fmt.Errorf("outlet declaration: access %q must be %q or %q", d.Access, AccessRead, AccessWrite)
	}
	if d.MaxRows < 0 {
		return nil, fmt.Errorf("outlet declaration: max_rows must not be negative")
	}
	if d.Timeout < 0 {
		return nil, fmt.Errorf("outlet declaration: timeout must not be negative (milliseconds)")
	}
	return &d, nil
}

// Writable reports whether outlet://<name>/exec is allowed on this outlet.
func (d *Decl) Writable() bool { return d.Access == AccessWrite }

// Hash fingerprints a declaration body. It is part of the pool key, so a
// redeploy that changes the declaration resolves to a new pool.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DeclNames returns the keys of a declaration map, sorted — for "unknown
// outlet X, have [a b c]" messages and deterministic validation order.
func DeclNames(decls map[string]*Decl) []string {
	names := make([]string, 0, len(decls))
	for n := range decls {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
