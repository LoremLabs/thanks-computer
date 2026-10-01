package server

import (
	"context"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// txco://caps/list — the tenant's capability catalogue, for a rule.
//
// A stack declares each capability it answers as CAPS/<name>.yaml
// (chassis/capdecl): a description, the input it takes, and optionally the
// scope a call enters at (the stack's start otherwise). This op lists what
// the tenant's ACTIVE stacks declare, by name, so a stack that offers
// capabilities to a model — a tool list — reads the catalogue instead of
// carrying a copy of it:
//
//	EXEC "txco://caps/list" WITH into = "_caps", prefix = "local."
//
//	_caps = {count, items: [{name, stack, entry, stage, description,
//	                         input: {<field>: {description, required}},
//	                         params: [<field>, …], timeout}]}
//
// `prefix` narrows by name (optional). `stage` is "<stack>/<entry>" —
// "<stack>/0", the stack's start, when none is declared — the shape a stage
// jump takes, so a stack on the same chassis can run a
// capability by `@goto` and one on another chassis can call it by
// `cap://<name>`. The catalogue says what exists, not what a run may call:
// that is the run grant's allowlist and the `_grant` stack's decision.
//
// A declaration that does not resolve or parse is left out of `items` and
// named under `broken: [{name, stack, error}]`. On failure:
// `<into>.error.{code,message}` (txco_caps_no_tenant, txco_caps_store).

// capLister is what the op reads the catalogue through.
type capLister interface {
	List(ctx context.Context, tenant string) ([]capEntry, error)
}

type capsDeps struct {
	decls capLister
}

type capOut struct {
	Name        string                        `json:"name"`
	Stack       string                        `json:"stack"`
	Entry       int                           `json:"entry"`
	Stage       string                        `json:"stage"`
	Description string                        `json:"description"`
	Input       map[string]capdecl.InputField `json:"input"`
	Params      []string                      `json:"params"`
	Timeout     int                           `json:"timeout"`
}

type capBrokenOut struct {
	Name  string `json:"name"`
	Stack string `json:"stack"`
	Error string `json:"error"`
}

type capsOut struct {
	Count  int            `json:"count"`
	Items  []capOut       `json:"items"`
	Broken []capBrokenOut `json:"broken,omitempty"`
}

func capsList(ctx context.Context, d capsDeps, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, "_caps")
	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		return identityErr(into, "caps", "no_tenant", "no tenant in request scope"), nil
	}
	if d.decls == nil {
		return identityErr(into, "caps", "store", "no capability declarations on this node"), nil
	}
	entries, err := d.decls.List(ctx, tenant)
	if err != nil {
		return identityErr(into, "caps", "store", err.Error()), nil
	}
	prefix := strings.TrimSpace(gjson.GetBytes(meta, "prefix").String())
	out := capsOut{Items: []capOut{}}
	for _, e := range entries {
		if prefix != "" && !strings.HasPrefix(e.Name, prefix) {
			continue
		}
		if e.Err != nil || e.Decl == nil {
			msg := "declaration could not be read"
			if e.Err != nil {
				msg = e.Err.Error()
			}
			out.Broken = append(out.Broken, capBrokenOut{Name: e.Name, Stack: e.Stack, Error: msg})
			continue
		}
		input := e.Decl.Input
		if input == nil {
			input = map[string]capdecl.InputField{}
		}
		out.Items = append(out.Items, capOut{
			Name: e.Name, Stack: e.Stack, Entry: e.Decl.Entry, Stage: e.Decl.Stage(e.Stack),
			Description: e.Decl.Description, Input: input, Params: e.Decl.Params(), Timeout: e.Decl.Timeout,
		})
	}
	out.Count = len(out.Items)
	return identityOK(into, out), nil
}
