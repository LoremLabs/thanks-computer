package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Ceilings on one declaration. A run grant holds at most a handful of
// sandboxes, each a handful of variables; chassis/authn bounds the row.
const (
	// MaxEnv bounds the variables one sandbox sets.
	MaxEnv = 32
	// MaxCapabilities bounds the capabilities one sandbox names.
	MaxCapabilities = 32
	// MaxDescription bounds the free-text note.
	MaxDescription = 256
)

// Decl is one SANDBOXES/<name>.yaml: what a program that opens the sandbox
// is handed, under which names, and what it may CALL. Only what the chassis
// enforces is declared here; a key it does not enforce is a deploy error,
// not a promise.
//
//	description: what a deploy step holds   # optional
//	env:
//	  GH_TOKEN: secret:GITHUB_PAT           # variable: reference
//	capabilities:                           # what a run may call on this
//	  - ai.chat                             # chassis, by bare name
//	  - card.note
//
// A sandbox sets variables, names capabilities, or both; one that does
// neither is a deploy error. A capability is not handed over: a run grant
// that names the sandbox carries the names in its allowlist, and the call
// is decided when it is made (chassis/server/grantgw).
//
// Parsed strictly: an unknown key is a deploy error, not a silent ignore.
type Decl struct {
	Description  string            `yaml:"description"`
	Env          map[string]string `yaml:"env"`
	Capabilities []string          `yaml:"capabilities"`
}

// Reference kinds. A reference is `<kind>:<name>`; `secret:` is the one
// kind today, the secret store's name grammar (chassis/secrets).
const KindSecret = "secret"

// ErrNotDeclared: the stack's active version has no SANDBOXES/<name>.yaml.
var ErrNotDeclared = errors.New("sandbox: not declared")

var (
	// varRe is what a process environment can carry, minus the shell's
	// own surprises: a letter or underscore, then letters, digits and _.
	varRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// secretNameRe mirrors the secret store's name rule (chassis/secrets:
	// ErrInvalidName) so a declaration can't reference a name the store
	// could never hold.
	secretNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,127}$`)
	// CapabilityRE is the grammar of a capability's name: lowercase words
	// joined by dots, such as `crm.lookup`. It is the one grammar for the
	// name everywhere it appears — here, in a standing grant and in a run
	// grant's allowlist (chassis/authn reads it from here).
	CapabilityRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}(\.[a-z][a-z0-9_-]{0,63}){0,7}$`)
)

// ReservedVarPrefix names the variables the chassis sets itself when it
// starts a program under a run grant (TXCO_RUN, TXCO_RUN_GRANT, …). A
// sandbox may not set one.
const ReservedVarPrefix = "TXCO_"

// ValidVar reports whether s may name a variable a sandbox sets.
func ValidVar(s string) bool {
	return varRe.MatchString(s) && !strings.HasPrefix(s, ReservedVarPrefix)
}

// ValidCapability reports whether s is a capability name.
func ValidCapability(s string) bool { return CapabilityRE.MatchString(s) }

// ParseRef reads a reference: `secret:GITHUB_PAT` → ("secret", "GITHUB_PAT").
func ParseRef(s string) (kind, name string, err error) {
	kind, name, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return "", "", fmt.Errorf("reference %q: want secret:<NAME>", s)
	}
	switch kind {
	case KindSecret:
		if !secretNameRe.MatchString(name) {
			return "", "", fmt.Errorf("reference %q: a secret name is a letter, then letters, digits and _ (1-128 chars)", s)
		}
	default:
		return "", "", fmt.Errorf("reference %q: unknown kind %q (want %s:<NAME>)", s, kind, KindSecret)
	}
	return kind, name, nil
}

// ParseDecl strictly decodes and validates a declaration body.
func ParseDecl(data []byte) (*Decl, error) {
	var d Decl
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("sandbox declaration: %w", err)
	}
	if len(d.Description) > MaxDescription {
		return nil, fmt.Errorf("sandbox declaration: description is longer than %d characters", MaxDescription)
	}
	if len(d.Env) == 0 && len(d.Capabilities) == 0 {
		return nil, fmt.Errorf("sandbox declaration: env or capabilities is required (at least one VARIABLE: secret:NAME, or one capability name)")
	}
	if len(d.Env) > MaxEnv {
		return nil, fmt.Errorf("sandbox declaration: env sets %d variables, at most %d", len(d.Env), MaxEnv)
	}
	if len(d.Capabilities) > MaxCapabilities {
		return nil, fmt.Errorf("sandbox declaration: capabilities names %d, at most %d", len(d.Capabilities), MaxCapabilities)
	}
	for v, ref := range d.Env {
		if !ValidVar(v) {
			if strings.HasPrefix(v, ReservedVarPrefix) {
				return nil, fmt.Errorf("sandbox declaration: env.%s: names starting %s are the chassis's own", v, ReservedVarPrefix)
			}
			return nil, fmt.Errorf("sandbox declaration: env.%s: a variable is a letter or _, then letters, digits and _ (1-128 chars)", v)
		}
		if _, _, err := ParseRef(ref); err != nil {
			return nil, fmt.Errorf("sandbox declaration: env.%s: %w", v, err)
		}
	}
	seen := map[string]bool{}
	for i, c := range d.Capabilities {
		c = strings.TrimSpace(c)
		if !ValidCapability(c) {
			return nil, fmt.Errorf("sandbox declaration: capabilities[%d]: %q is not a capability name (lowercase words joined by dots, such as crm.lookup)", i, c)
		}
		if seen[c] {
			return nil, fmt.Errorf("sandbox declaration: capabilities names %q twice", c)
		}
		seen[c] = true
		d.Capabilities[i] = c
	}
	return &d, nil
}

// Vars is the sorted list of variables the sandbox sets.
func (d *Decl) Vars() []string {
	out := make([]string, 0, len(d.Env))
	for v := range d.Env {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Secrets is the sorted, deduplicated list of secret names the sandbox
// releases: what a run grant that names the sandbox must be allowed to
// release.
func (d *Decl) Secrets() []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range d.Env {
		kind, name, err := ParseRef(ref)
		if err != nil || kind != KindSecret || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CapabilityNames is the sorted list of capabilities the sandbox names:
// what a run grant that names the sandbox may call.
func (d *Decl) CapabilityNames() []string {
	out := append([]string(nil), d.Capabilities...)
	sort.Strings(out)
	return out
}

// DeclNames returns the keys of a declaration map, sorted — for "unknown
// sandbox X, have [a b c]" messages and deterministic validation order.
func DeclNames(decls map[string]*Decl) []string {
	names := make([]string, 0, len(decls))
	for n := range decls {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
