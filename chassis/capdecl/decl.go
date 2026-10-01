package capdecl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// Ceilings on one declaration.
const (
	// MaxInputs bounds the inputs one capability names.
	MaxInputs = 32
	// MaxDescription bounds the capability's description: it is what a
	// model reads to decide whether to call, so it is longer than a note.
	MaxDescription = 2048
	// MaxInputDescription bounds one input's description.
	MaxInputDescription = 512
	// MaxTimeout bounds the declared timeout, in milliseconds. The inlet's
	// own ceiling still applies; a declaration can only shorten the wait.
	MaxTimeout = 3600000
)

// InputField is one input a capability takes.
type InputField struct {
	Description string `yaml:"description" json:"description,omitempty"`
	Required    bool   `yaml:"required" json:"required,omitempty"`
}

// Decl is one CAPS/<name>.yaml: who answers a capability and what it takes.
// The file's existence is the declaration — the stack that holds it answers
// the capability — and every key is optional. Only what the chassis reads
// is declared here; a key it does not read is a deploy error, not a promise.
//
//	description: Send a message by email.   # what a caller reads
//	input:                                  # what the call carries
//	  to:
//	    description: the recipient's address
//	    required: true
//	  subject: {}
//	timeout: 60000                          # ms; shortens the wait
//	entry: 7000                             # see below
//
// A call ENTERS THE STACK AT ITS START, `<stack>/0`, like every other
// inlet's run (`_cron/0`, `_mail/0`): the stack's scopes run in order and
// their WHENs pick the call up (`@src == "cap"`, `@cap.name`). `entry` is
// for a stack that does other work too and wants a capability's run to
// begin further in: it names the scope of THIS stack the run starts from.
//
// Parsed strictly: an unknown key is a deploy error, not a silent ignore.
type Decl struct {
	Description string                `yaml:"description"`
	Entry       int                   `yaml:"entry"`
	Input       map[string]InputField `yaml:"input"`
	Timeout     int                   `yaml:"timeout"`
}

// ErrNotDeclared: no active stack of the tenant has a CAPS/<name>.yaml.
var ErrNotDeclared = errors.New("cap: not declared")

// inputRe pins input names: they become keys of the call's input and
// parameter names a model writes, so lowercase identifiers only.
var inputRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidInput reports whether s may name an input.
func ValidInput(s string) bool { return inputRe.MatchString(s) }

// ParseDecl strictly decodes and validates a declaration body.
func ParseDecl(data []byte) (*Decl, error) {
	var d Decl
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// An empty file (or one of comments only) declares the capability with
	// every default: the decoder reports it as EOF.
	if err := dec.Decode(&d); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("capability declaration: %w", err)
	}
	if len(d.Description) > MaxDescription {
		return nil, fmt.Errorf("capability declaration: description is longer than %d characters", MaxDescription)
	}
	if d.Entry < 0 {
		return nil, fmt.Errorf("capability declaration: entry is %d, want a scope of this stack (omit it to enter at the stack's start)", d.Entry)
	}
	if len(d.Input) > MaxInputs {
		return nil, fmt.Errorf("capability declaration: input names %d fields, at most %d", len(d.Input), MaxInputs)
	}
	for name, f := range d.Input {
		if !ValidInput(name) {
			return nil, fmt.Errorf("capability declaration: input.%s: an input is a lowercase letter, then lowercase letters, digits and _ (1-64 chars)", name)
		}
		if len(f.Description) > MaxInputDescription {
			return nil, fmt.Errorf("capability declaration: input.%s: description is longer than %d characters", name, MaxInputDescription)
		}
	}
	if d.Timeout < 0 || d.Timeout > MaxTimeout {
		return nil, fmt.Errorf("capability declaration: timeout is %d, want milliseconds between 0 and %d", d.Timeout, MaxTimeout)
	}
	return &d, nil
}

// Stage is where a call enters: "<stack>/<entry>", the shape a stage jump
// takes (processor.StagePartsRE) — "<stack>/0", the stack's start, when no
// entry is declared.
func (d *Decl) Stage(stack string) string { return stack + "/" + strconv.Itoa(d.Entry) }

// Params is the sorted list of the inputs the capability names.
func (d *Decl) Params() []string {
	out := make([]string, 0, len(d.Input))
	for n := range d.Input {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// TimeoutDuration is the declared timeout; zero means the inlet's ceiling.
func (d *Decl) TimeoutDuration() time.Duration {
	return time.Duration(d.Timeout) * time.Millisecond
}

// CheckEntry reports whether the declaration's entry names a scope the
// stack has. scopes is the set of scope numbers of the declaring stack. No
// entry is the stack's start, which any stack with a scope has.
func CheckEntry(d *Decl, scopes map[int]bool) error {
	if d.Entry == 0 {
		if len(scopes) == 0 {
			return fmt.Errorf("capability declaration: this stack has no scope to answer the call")
		}
		return nil
	}
	if !scopes[d.Entry] {
		return fmt.Errorf("capability declaration: entry %d names no scope of this stack", d.Entry)
	}
	return nil
}
