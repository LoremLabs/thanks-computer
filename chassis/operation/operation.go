package operation

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

var PathToOpRE = regexp.MustCompile(`OPS/(?P<stack>(?:\$boot)|(?:\$service(?:\/\$slot)*)).*?/?(?P<scope>\d+)\D*\/(?P<file>(?:resonator.txcl))$`)

type Operation struct {
	Input     string `json:"input,omitempty"`
	Output    string `json:"output,omitempty"`
	Service   string `json:"service,omitempty"`
	Slot      string `json:"slot,omitempty"`
	Stack     string `json:"stack,omitempty"`
	Scope     int    `json:"scope,omitempty"`
	Name      string `json:"name,omitempty"` // filename-derived identity within (stack, scope); empty for legacy rules
	Txcl      string `json:"txcl,omitempty"`
	MockRes   string `json:"mockRes,omitempty"`
	MockReq   string `json:"mockReq,omitempty"`
	Resonator *resonator.Resonator
	Meta      string `json:"meta,omitempty"`
	OpID      string `json:"opId,omitempty"` // unique ID for this Operation

	// Secrets holds materialized secret cleartext for this op
	// instance. The field is tagged `json:"-"` AND its type's
	// MarshalJSON / MarshalText / GobEncode all panic, so cleartext
	// cannot reach any envelope, trace, log, mock, or continuation
	// by construction (internal docs/todo-secret-store.md §4.1).
	//
	// Populated by the processor (PR 3) between WHEN/SET/SELECT/WITH
	// decoration and Exec; zeroed via defer on every exit path. Read
	// directly by secret-aware op handlers as op.Secrets.Get(NAME);
	// handlers never call the Resolver or Store themselves.
	Secrets secrets.SecretBag `json:"-"`
}

// New Create a new Operation
func New() *Operation {

	var o = &Operation{}
	o.OpID = hxid.NewTimeSort().String()

	return o
}

// Copy Create a shallow copy of the Operation, forcing a new OpID.
//
// Secrets bag is shared by reference (SecretBag's internal map is
// not deep-copied). Copy() is called within a single request scope
// to spawn a new execution instance; the copy and the original both
// see the same materialized cleartext, and the processor's deferred
// Zero() wipes both views in one call. Sharing is correct here.
func (op *Operation) Copy() *Operation {

	// NB: careful of the Resonator
	var o = &Operation{Input: op.Input, Output: op.Output, Service: op.Service, Slot: op.Slot, Stack: op.Stack, Scope: op.Scope, Name: op.Name, Txcl: op.Txcl, MockRes: op.MockRes, MockReq: op.MockReq, Resonator: op.Resonator, Meta: op.Meta, Secrets: op.Secrets}
	o.OpID = hxid.NewTimeSort().String()

	return o
}

func PathToOperation(path string) (*Operation, error) {

	// get operation's stack and scope from path
	// APPS/readme.md
	// OPS/$boot/0
	// OPS/$boot/0/cron/resonator.txcl
	// OPS/$service
	// OPS/$service/$slot/0000_SETUP/0001_HELLO/resonator.txcl
	// OPS/$service/1/mock-response.yaml

	if !PathToOpRE.MatchString(path) {
		return nil, errors.New("not txcl")
	}

	match := PathToOpRE.FindStringSubmatch(path)
	result := make(map[string]string)
	for i, name := range PathToOpRE.SubexpNames() {
		if i != 0 && name != "" {
			result[name] = match[i]
		}
	}

	scope, _ := strconv.Atoi(result["scope"])

	return &Operation{
		Scope: scope,
		Stack: result["stack"],
	}, nil
}

// --- per-op context plumbing ---
//
// Chassis-core ops dispatched via the OpsHandler interface only get
// (ctx, opName, in, out) — no access to the full Operation struct.
// For ops that read parameters from op.Meta (the WITH-clause channel,
// e.g. txco://hmac-sign reads algorithm/input_path/output_path), the
// processor's ExecCore stashes meta on ctx before calling the handler.
// Mirrors the secrets.WithBag pattern.

type ctxKeyMeta struct{}

// WithMeta attaches the op's Meta string to ctx for downstream
// OpsHandler consumption. Empty meta is a no-op.
func WithMeta(ctx context.Context, meta string) context.Context {
	if meta == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyMeta{}, meta)
}

// MetaFromContext returns the meta string set by WithMeta, or "" if
// none was attached. Empty return is safe to gjson.Get against.
func MetaFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKeyMeta{}).(string); ok {
		return v
	}
	return ""
}

func (op *Operation) MacroExpand(input string, service string, slot string) (output string) {

	// replace macros with values for this push
	output = strings.ReplaceAll(input, "$boot", "boot/"+service)
	output = strings.ReplaceAll(output, "$service/$slot", service+"/"+slot) // order matters
	output = strings.ReplaceAll(output, "$service", service)
	output = strings.ReplaceAll(output, "$slot", slot)

	return output
}
