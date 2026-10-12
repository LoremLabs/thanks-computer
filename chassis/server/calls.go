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

// txco://calls/list — the entries the tenant's stacks declare, for a rule.
//
// A stack declares each entry a call may name as CALLS/<name>.yaml
// (chassis/calldecl): where the call enters, the input it takes, the output
// it answers with, a timeout. This op lists what the tenant's ACTIVE stacks
// declare, so a stack that composes others — or a model choosing a sub-flow
// — reads the contracts instead of carrying a copy:
//
//	EXEC "txco://calls/list" WITH into = "_calls", stack = "app/_orient"
//
//	_calls = {count, items: [{stack, name, entry, stage, description,
//	                          input: {<field>: {description, required}},
//	                          output: {<field>: {description, required}},
//	                          params: [<field>, …], returns: [<field>, …],
//	                          timeout}]}
//
// `stack` narrows to one stack (optional). `stage` is "<stack>/<entry>".
// A declaration that does not resolve or parse is left out of `items` and
// named under `broken: [{stack, name, error}]`. On failure:
// `<into>.error.{code,message}` (txco_calls_no_tenant, txco_calls_store).

// callLister is what the op reads the entries through.
type callLister interface {
	List(ctx context.Context, tenant string) ([]callEntry, error)
}

type callsDeps struct {
	decls callLister
}

type callListOut struct {
	Stack       string                   `json:"stack"`
	Name        string                   `json:"name"`
	Entry       int                      `json:"entry"`
	Stage       string                   `json:"stage"`
	Description string                   `json:"description"`
	Input       map[string]capdecl.Field `json:"input"`
	Output      map[string]capdecl.Field `json:"output"`
	Params      []string                 `json:"params"`
	Returns     []string                 `json:"returns"`
	Timeout     int                      `json:"timeout"`
}

type callListBroken struct {
	Stack string `json:"stack"`
	Name  string `json:"name"`
	Error string `json:"error"`
}

type callsListOut struct {
	Count  int              `json:"count"`
	Items  []callListOut    `json:"items"`
	Broken []callListBroken `json:"broken,omitempty"`
}

func callsList(ctx context.Context, d callsDeps, _ []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, "_calls")
	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		return identityErr(into, "calls", "no_tenant", "no tenant in request scope"), nil
	}
	if d.decls == nil {
		return identityErr(into, "calls", "store", "no call declarations on this node"), nil
	}
	entries, err := d.decls.List(ctx, tenant)
	if err != nil {
		return identityErr(into, "calls", "store", err.Error()), nil
	}
	only := strings.TrimSpace(gjson.GetBytes(meta, "stack").String())
	out := callsListOut{Items: []callListOut{}}
	for _, e := range entries {
		if only != "" && e.Stack != only {
			continue
		}
		if e.Err != nil || e.Decl == nil {
			msg := "declaration could not be read"
			if e.Err != nil {
				msg = e.Err.Error()
			}
			out.Broken = append(out.Broken, callListBroken{Stack: e.Stack, Name: e.Name, Error: msg})
			continue
		}
		input, output := e.Decl.Input, e.Decl.Output
		if input == nil {
			input = map[string]capdecl.Field{}
		}
		if output == nil {
			output = map[string]capdecl.Field{}
		}
		out.Items = append(out.Items, callListOut{
			Stack: e.Stack, Name: e.Name, Entry: e.Decl.Entry, Stage: e.Decl.Stage(e.Stack),
			Description: e.Decl.Description, Input: input, Output: output,
			Params: e.Decl.Params(), Returns: e.Decl.Returns(), Timeout: e.Decl.Timeout,
		})
	}
	out.Count = len(out.Items)
	return identityOK(into, out), nil
}
