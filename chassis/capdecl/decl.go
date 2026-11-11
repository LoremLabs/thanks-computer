package capdecl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
	yaml "go.yaml.in/yaml/v3"

	"github.com/loremlabs/thanks-computer/chassis/jsonschemax"
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

// Field is one input a capability takes, or one output it answers with:
// what a caller reads (the description), whether a call must carry it, and
// optionally its shape, with the keys JSON Schema uses (field.go). A field
// with no typed key accepts any value.
//
//	query:
//	  type: string                 # or [string, "null"] for a value that may be null
//	  max_length: 500
//	  description: a few words
//	  required: true
//	count:
//	  type: integer
//	  minimum: 1
//	  maximum: 20
//	  default: 10                  # an annotation: what the stack assumes; never filled in
//	tags:
//	  type: array
//	  items: {type: string}
//	options:
//	  type: object
//	  fields: {verbose: {type: boolean}}
//	part:
//	  schema: {anyOf: [{type: integer}, {type: string, enum: [last]}]}   # raw JSON Schema, alone
type Field struct {
	Description string           `yaml:"description" json:"description,omitempty"`
	Required    bool             `yaml:"required" json:"required,omitempty"`
	Type        TypeList         `yaml:"type" json:"type,omitempty"`
	Enum        []any            `yaml:"enum" json:"enum,omitempty"`
	Default     any              `yaml:"default" json:"default,omitempty"`
	Minimum     *float64         `yaml:"minimum" json:"minimum,omitempty"`
	Maximum     *float64         `yaml:"maximum" json:"maximum,omitempty"`
	MinLength   *int             `yaml:"min_length" json:"min_length,omitempty"`
	MaxLength   *int             `yaml:"max_length" json:"max_length,omitempty"`
	Pattern     string           `yaml:"pattern" json:"pattern,omitempty"`
	Items       *Field           `yaml:"items" json:"items,omitempty"`
	Fields      map[string]Field `yaml:"fields" json:"fields,omitempty"`
	Schema      map[string]any   `yaml:"schema" json:"schema,omitempty"`
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
// `effect` says what a call of the capability does to the world outside the
// run that makes it: `read` or `write`. It is what a caller that makes
// several calls needs to know before it decides how to make them, and it is
// a PROMISE of the stack's, not something the chassis checks:
//
//	read   the call can run beside other calls, and it can run again. It
//	       changes nothing a second call, or a neighbour's call, would see:
//	       no record written, nothing sent, nothing marked as seen.
//	write  anything else. A capability that keeps only one of the two
//	       promises of `read` is a write.
//
// Absent, the declaration says nothing, and the catalogue reports nothing:
// what silence means is the reader's to decide (a careful one reads it as
// write). The chassis itself reads the key nowhere: every call is decided
// and run the same whichever it says. A call entry (CALLS/) is not a tool
// and takes no `effect` (calldecl refuses it).
//
// Parsed strictly: an unknown key is a deploy error, not a silent ignore.
type Decl struct {
	Description string           `yaml:"description"`
	Entry       int              `yaml:"entry"`
	Input       map[string]Field `yaml:"input"`
	Output      map[string]Field `yaml:"output"`
	Timeout     int              `yaml:"timeout"`
	Effect      string           `yaml:"effect"`

	// The derived schemas (field.go), filled at parse so a declaration that
	// yields a schema the compiler refuses is a deploy error, and reused by
	// every catalogue read of the cached declaration.
	inputSchema  json.RawMessage
	outputSchema json.RawMessage
}

// The two things a declaration's `effect` may say.
const (
	EffectRead  = "read"
	EffectWrite = "write"
)

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
	for _, which := range []string{"input", "output"} {
		fields := d.Input
		if which == "output" {
			fields = d.Output
		}
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if !ValidInput(name) {
				return nil, fmt.Errorf("%s: %s.%s: a field is a lowercase letter, then lowercase letters, digits and _ (1-64 chars)", kind, which, name)
			}
			if err := checkField(kind, which+"."+name, fields[name], 0); err != nil {
				return nil, err
			}
		}
	}
	if d.Timeout < 0 || d.Timeout > MaxTimeout {
		return nil, fmt.Errorf("%s: timeout is %d, want milliseconds between 0 and %d", kind, d.Timeout, MaxTimeout)
	}
	if d.Effect != "" && d.Effect != EffectRead && d.Effect != EffectWrite {
		return nil, fmt.Errorf("%s: effect is %q, want %q or %q (omit it to say nothing)", kind, d.Effect, EffectRead, EffectWrite)
	}
	// The derived schemas must compile: a `schema:` hatch or a pattern the
	// compiler refuses is caught here, at lint, apply and activation.
	for _, which := range []string{"input", "output"} {
		raw := d.InputSchema()
		if which == "output" {
			raw = d.OutputSchema()
		}
		if _, err := jsonschemax.Compile(raw); err != nil {
			return nil, fmt.Errorf("%s: %s: the derived schema does not compile: %v", kind, which, err)
		}
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
// object, lacks: absent, or present as null, unless the field's type
// allows null (`type: [string, "null"]`), in which case an explicit null is
// a value. An empty list means doc has every field the declaration
// requires; it says nothing else about their shape, which is a schema
// check's (InputSchema with jsonschemax).
func Missing(fields map[string]Field, doc string) []string {
	var out []string
	for name, f := range fields {
		if !f.Required {
			continue
		}
		v := gjson.Get(doc, name)
		if !v.Exists() || (v.Type == gjson.Null && !f.Type.Has("null")) {
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
