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
	// MaxFiles bounds the files one sandbox serves.
	MaxFiles = 8
	// MaxPath bounds a file's path.
	MaxPath = 256
)

// Decl is one SANDBOXES/<name>.yaml: what a program that opens the sandbox
// is handed, under which names, and what it may CALL. Only what the chassis
// enforces is declared here; a key it does not enforce is a deploy error,
// not a promise.
//
//	description: what a deploy step holds   # optional
//	env:
//	  GH_TOKEN: secret:GITHUB_PAT           # variable: reference
//	  GITHUB: connection:github             # a principal's token at a provider
//	files:                                  # path: reference, served as a file
//	  ~/.ssh/id_ed25519: secret:DEPLOY_KEY
//	dotenv: run.env                         # the env entries, also as a file
//	capabilities:                           # what a run may call on this
//	  - ai.chat                             # chassis, by bare name
//	  - card.note
//
// A sandbox sets variables, serves files, names capabilities, or any of
// them; one that does none is a deploy error. A file's value is the whole
// reference's value (a multi-line key is a file, not a variable); the
// program that opens the sandbox reads it at the path, relative to the
// directory it serves in, or under ~/. `dotenv` serves the env entries as
// NAME=value lines at that path, for a program that reads a .env file. A capability is not handed over: a run grant
// that names the sandbox carries the names in its allowlist, and the call
// is decided when it is made (chassis/server/grantgw).
//
// Parsed strictly: an unknown key is a deploy error, not a silent ignore.
type Decl struct {
	Description  string            `yaml:"description"`
	Env          map[string]string `yaml:"env"`
	Files        map[string]string `yaml:"files"`
	Dotenv       string            `yaml:"dotenv"`
	Capabilities []string          `yaml:"capabilities"`
}

// Reference kinds. A reference is `<kind>:<name>`: `secret:` names a
// tenant secret (the secret store's name grammar, chassis/secrets);
// `connection:` names a provider whose token the principal holds
// (chassis/connection), released as the token itself.
const (
	KindSecret     = "secret"
	KindConnection = "connection"
)

// Entry keys. A run grant snapshots a sandbox as one map, entry → value:
// a variable by its name, a file as `file:<path>` (value: its reference)
// and the dotenv file as `dotenv:<path>` (value: DotenvValue). A variable
// name holds no colon, so the three never collide.
const (
	FilePrefix   = "file:"
	DotenvPrefix = "dotenv:"
	DotenvValue  = "env"
)

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
	// providerNameRe mirrors chassis/connection's provider name rule.
	providerNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	// pathPartRe is one part of a file's path: no space, no shell or
	// glob surprise, never "." or "..".
	pathPartRe = regexp.MustCompile(`^[A-Za-z0-9._@+-]{1,128}$`)
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

// ParseRef reads a reference: `secret:GITHUB_PAT` → ("secret", "GITHUB_PAT"),
// `connection:github` → ("connection", "github").
func ParseRef(s string) (kind, name string, err error) {
	kind, name, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return "", "", fmt.Errorf("reference %q: want secret:<NAME> or connection:<provider>", s)
	}
	switch kind {
	case KindSecret:
		if !secretNameRe.MatchString(name) {
			return "", "", fmt.Errorf("reference %q: a secret name is a letter, then letters, digits and _ (1-128 chars)", s)
		}
	case KindConnection:
		if !providerNameRe.MatchString(name) {
			return "", "", fmt.Errorf("reference %q: a provider name is a lowercase letter, then lowercase letters, digits, _ and - (1-64 chars)", s)
		}
	default:
		return "", "", fmt.Errorf("reference %q: unknown kind %q (want %s:<NAME> or %s:<provider>)", s, kind, KindSecret, KindConnection)
	}
	return kind, name, nil
}

// ValidPath reports whether p may name a file a sandbox serves: relative
// to the directory it is served in, or under the home directory (`~/`);
// parts of letters, digits and . _ @ + -, never "." or ".."; no leading /.
func ValidPath(p string) bool {
	if p == "" || len(p) > MaxPath {
		return false
	}
	rest := strings.TrimPrefix(p, "~/")
	parts := strings.Split(rest, "/")
	if len(parts) > 8 {
		return false
	}
	for _, part := range parts {
		if part == "." || part == ".." || !pathPartRe.MatchString(part) {
			return false
		}
	}
	return true
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
	if len(d.Env) == 0 && len(d.Files) == 0 && len(d.Capabilities) == 0 {
		return nil, fmt.Errorf("sandbox declaration: env, files or capabilities is required (at least one VARIABLE: secret:NAME, one path: secret:NAME, or one capability name)")
	}
	if len(d.Files) > MaxFiles {
		return nil, fmt.Errorf("sandbox declaration: files serves %d, at most %d", len(d.Files), MaxFiles)
	}
	for p, ref := range d.Files {
		if !ValidPath(p) {
			return nil, fmt.Errorf("sandbox declaration: files.%s: a path is relative or under ~/, of parts made of letters, digits and . _ @ + - (never . or ..), at most %d characters", p, MaxPath)
		}
		if _, _, err := ParseRef(ref); err != nil {
			return nil, fmt.Errorf("sandbox declaration: files.%s: %w", p, err)
		}
	}
	if d.Dotenv != "" {
		if len(d.Env) == 0 {
			return nil, fmt.Errorf("sandbox declaration: dotenv %s: the sandbox sets no variable to put in it", d.Dotenv)
		}
		if !ValidPath(d.Dotenv) {
			return nil, fmt.Errorf("sandbox declaration: dotenv %s: a path is relative or under ~/, of parts made of letters, digits and . _ @ + - (never . or ..), at most %d characters", d.Dotenv, MaxPath)
		}
		if _, dup := d.Files[d.Dotenv]; dup {
			return nil, fmt.Errorf("sandbox declaration: dotenv %s is also one of files", d.Dotenv)
		}
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

// Entries is the sandbox as a run grant snapshots it: every variable by
// its name, every file as `file:<path>`, the dotenv file as
// `dotenv:<path>` (see FilePrefix).
func (d *Decl) Entries() map[string]string {
	out := make(map[string]string, len(d.Env)+len(d.Files)+1)
	for v, ref := range d.Env {
		out[v] = ref
	}
	for p, ref := range d.Files {
		out[FilePrefix+p] = ref
	}
	if d.Dotenv != "" {
		out[DotenvPrefix+d.Dotenv] = DotenvValue
	}
	return out
}

// Secrets is the sorted, deduplicated list of secret names the sandbox
// releases: what a run grant that names the sandbox must be allowed to
// release.
func (d *Decl) Secrets() []string {
	seen := map[string]bool{}
	var out []string
	for _, ref := range d.Entries() {
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

// Entry kinds, as a run grant's snapshot names them.
const (
	EntryVar    = "var"
	EntryFile   = "file"
	EntryDotenv = "dotenv"
)

// ParseEntry reads one key of a snapshot: its kind and, for a file or the
// dotenv file, its path. ok is false for a key that is none of the three.
func ParseEntry(key string) (kind, path string, ok bool) {
	switch {
	case strings.HasPrefix(key, FilePrefix):
		p := key[len(FilePrefix):]
		return EntryFile, p, ValidPath(p)
	case strings.HasPrefix(key, DotenvPrefix):
		p := key[len(DotenvPrefix):]
		return EntryDotenv, p, ValidPath(p)
	default:
		return EntryVar, "", ValidVar(key)
	}
}

// CheckEntry validates one entry of a snapshot: a variable or a file with
// a reference, or the dotenv file with DotenvValue.
func CheckEntry(key, value string) error {
	kind, _, ok := ParseEntry(key)
	if !ok {
		return fmt.Errorf("%q is not a variable, file or dotenv entry a sandbox may have", key)
	}
	if kind == EntryDotenv {
		if value != DotenvValue {
			return fmt.Errorf("%s: want %q", key, DotenvValue)
		}
		return nil
	}
	if _, _, err := ParseRef(value); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}
