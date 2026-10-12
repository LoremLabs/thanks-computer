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

	"github.com/tidwall/gjson"
	yaml "go.yaml.in/yaml/v3"
)

// Ceilings on one declaration.
const (
	// MaxInputs bounds the inputs one capability names; MaxOutputs the
	// outputs it promises.
	MaxInputs  = 32
	MaxOutputs = 32
	// MaxDescription bounds the capability's description: it is what a
	// model reads to decide whether to call, so it is longer than a note.
	MaxDescription = 2048
	// MaxInputDescription bounds one input's or output's description.
	MaxInputDescription = 512
	// MaxTimeout bounds the declared timeout, in milliseconds. The inlet's
	// own ceiling still applies; a declaration can only shorten the wait.
	MaxTimeout = 3600000
)

// Field is one input a capability takes, or one output it answers with.
type Field struct {
	Description string `yaml:"description" json:"description,omitempty"`
	Required    bool   `yaml:"required" json:"required,omitempty"`
}

// InputField is the name Field had when a declaration named inputs only.
type InputField = Field

// Decl is one CAPS/<name>.yaml: who answers a capability, what it takes and
// what it answers. The file's existence is the declaration — the stack that
// holds it answers the capability — and every key is optional. Only what
// the chassis reads is declared here; a key it does not read is a deploy
// error, not a promise.
//
//	description: Send a message by email.   # what a caller reads
//	input:                                  # what the call carries
//	  to:
//	    description: the recipient's address
//	    required: true
//	  subject: {}
//	output:                                 # what the answer carries
//	  mid:
//	    description: the message id
//	    required: true
//	timeout: 60000                          # ms; shortens the wait
//	entry: 7000                             # see below
//
// A call ENTERS THE STACK AT ITS START, `<stack>/0`, like every other
// inlet's run (`_cron/0`, `_mail/0`): the stack's scopes run in order and
// their WHENs pick the call up (`@src == "cap"`, `@cap.name`). `entry` is
// for a stack that does other work too and wants a capability's run to
// begin further in: it names the scope of THIS stack the run starts from.
//
// The same shape declares an ENTRY a call may name (CALLS/<name>.yaml,
// chassis/calldecl): there `input` is checked before the callee runs and
// `output` after it ends. On a capability, `output` is checked the same way
// when the answer comes; `input` is what a caller reads, and the stack's
// own rules decide what a call without it gets.
//
// Parsed strictly: an unknown key is a deploy error, not a silent ignore.
type Decl struct {
	Description string           `yaml:"description"`
	Entry       int              `yaml:"entry"`
	Input       map[string]Field `yaml:"input"`
	Output      map[string]Field `yaml:"output"`
	Timeout     int              `yaml:"timeout"`
}

// ErrNotDeclared: no active stack of the tenant has a CAPS/<name>.yaml.
var ErrNotDeclared = errors.New("cap: not declared")

// inputRe pins input and output names: they become keys of the call's
// input and of the answer, and parameter names a model writes, so lowercase
// identifiers only.
var inputRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidInput reports whether s may name an input or an output.
func ValidInput(s string) bool { return inputRe.MatchString(s) }

// ParseDecl strictly decodes and validates a capability declaration.
func ParseDecl(data []byte) (*Decl, error) { return ParseDeclAs("capability declaration", data) }

// ParseDeclAs is ParseDecl with the declaration named as the caller's: a
// call entry's errors say "call declaration".
func ParseDeclAs(kind string, data []byte) (*Decl, error) {
	var d Decl
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	// An empty file (or one of comments only) declares the capability with
	// every default: the decoder reports it as EOF.
	if err := dec.Decode(&d); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	if len(d.Description) > MaxDescription {
		return nil, fmt.Errorf("%s: description is longer than %d characters", kind, MaxDescription)
	}
	if d.Entry < 0 {
		return nil, fmt.Errorf("%s: entry is %d, want a scope of this stack (omit it to enter at the stack's start)", kind, d.Entry)
	}
	if len(d.Input) > MaxInputs {
		return nil, fmt.Errorf("%s: input names %d fields, at most %d", kind, len(d.Input), MaxInputs)
	}
	if len(d.Output) > MaxOutputs {
		return nil, fmt.Errorf("%s: output names %d fields, at most %d", kind, len(d.Output), MaxOutputs)
	}
	for which, fields := range map[string]map[string]Field{"input": d.Input, "output": d.Output} {
		for name, f := range fields {
			if !ValidInput(name) {
				return nil, fmt.Errorf("%s: %s.%s: a field is a lowercase letter, then lowercase letters, digits and _ (1-64 chars)", kind, which, name)
			}
			if len(f.Description) > MaxInputDescription {
				return nil, fmt.Errorf("%s: %s.%s: description is longer than %d characters", kind, which, name, MaxInputDescription)
			}
		}
	}
	if d.Timeout < 0 || d.Timeout > MaxTimeout {
		return nil, fmt.Errorf("%s: timeout is %d, want milliseconds between 0 and %d", kind, d.Timeout, MaxTimeout)
	}
	return &d, nil
}

// Stage is where a call enters: "<stack>/<entry>", the shape a stage jump
// takes (processor.StagePartsRE) — "<stack>/0", the stack's start, when no
// entry is declared.
func (d *Decl) Stage(stack string) string { return stack + "/" + strconv.Itoa(d.Entry) }

// Params is the sorted list of the inputs the capability names.
func (d *Decl) Params() []string { return names(d.Input) }

// Returns is the sorted list of the outputs the capability names.
func (d *Decl) Returns() []string { return names(d.Output) }

func names(fields map[string]Field) []string {
	out := make([]string, 0, len(fields))
	for n := range fields {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Missing is the sorted list of the required fields that doc, a JSON
// object, lacks: absent, or present as null. An empty list means doc has
// every field the declaration requires; it says nothing about their shape,
// which a declaration does not describe.
func Missing(fields map[string]Field, doc string) []string {
	var out []string
	for name, f := range fields {
		if !f.Required {
			continue
		}
		if v := gjson.Get(doc, name); !v.Exists() || v.Type == gjson.Null {
			out = append(out, name)
		}
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
	return CheckEntryAs("capability declaration", d, scopes)
}

// CheckEntryAs is CheckEntry with the declaration named as the caller's.
func CheckEntryAs(kind string, d *Decl, scopes map[int]bool) error {
	if d.Entry == 0 {
		if len(scopes) == 0 {
			return fmt.Errorf("%s: this stack has no scope to answer the call", kind)
		}
		return nil
	}
	if !scopes[d.Entry] {
		return fmt.Errorf("%s: entry %d names no scope of this stack", kind, d.Entry)
	}
	return nil
}
