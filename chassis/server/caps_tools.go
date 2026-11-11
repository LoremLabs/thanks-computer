package server

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// The catalogue as a tool list. `txco://caps/list WITH format = "tools"`
// answers what `ai://chat WITH tools` takes: one definition per
// capability, named for the model (capdecl.ToolName: `web.search` →
// `web_search`), with the declaration's derived input schema as its
// parameters and a binding that names the capability, so a dispatch rule
// runs `cap://` from the call's binding without a lookup. Nothing here is
// a second registry: the declarations are the source, read through the
// same catalogue every other reader uses.

// capsFilter narrows a listing: by prefix, by exact names, or both.
type capsFilter struct {
	prefix string
	names  map[string]bool // nil: no name filter
	seen   map[string]bool
	format string // "" (items) or "tools"
}

// capsFilterFrom reads prefix, names and format from the WITH meta.
func capsFilterFrom(meta []byte) (*capsFilter, error) {
	f := &capsFilter{prefix: strings.TrimSpace(gjson.GetBytes(meta, "prefix").String()), seen: map[string]bool{}}
	if r := gjson.GetBytes(meta, "names"); r.Exists() {
		if !r.IsArray() {
			return nil, fmt.Errorf("WITH names is an array of capability names")
		}
		f.names = map[string]bool{}
		for _, n := range r.Array() {
			if n.Type != gjson.String || strings.TrimSpace(n.String()) == "" {
				return nil, fmt.Errorf("WITH names is an array of capability names")
			}
			f.names[n.String()] = true
		}
	}
	switch format := gjson.GetBytes(meta, "format").String(); format {
	case "", "items":
	case "tools":
		f.format = "tools"
	default:
		return nil, fmt.Errorf("WITH format is \"tools\", or absent for the plain list")
	}
	return f, nil
}

// keep reports whether a capability of this name is listed.
func (f *capsFilter) keep(name string) bool {
	if f.prefix != "" && !strings.HasPrefix(name, f.prefix) {
		return false
	}
	if f.names != nil {
		if !f.names[name] {
			return false
		}
		f.seen[name] = true
	}
	return true
}

// missing is the names asked for that the catalogue does not hold, sorted:
// data, not an error, so a stack offers what exists.
func (f *capsFilter) missing() []string {
	if f.names == nil {
		return nil
	}
	var out []string
	for n := range f.names {
		if !f.seen[n] {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// capToolOut is one tool definition, in the shape ai://chat takes.
type capToolOut struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Binding     capToolBinding  `json:"binding"`
	// Effect is the declaration's `effect` (read or write), absent when the
	// declaration says nothing. For the executor, like the binding: how the
	// calls of one answer may be made (beside each other, again) is its to
	// decide, and this is what it decides from. A model is not shown it.
	Effect string `json:"effect,omitempty"`
}

// capToolBinding says what runs the tool: the capability, called with the
// model's arguments as its input. An executor keeps this as request
// metadata; ai://chat copies it onto each call and never reads it.
type capToolBinding struct {
	Capability string `json:"capability"`
}

type capsToolsOut struct {
	Count   int            `json:"count"`
	Tools   []capToolOut   `json:"tools"`
	Broken  []capBrokenOut `json:"broken,omitempty"`
	Missing []string       `json:"missing,omitempty"`
}

// capsListTools renders the filtered catalogue as tool definitions, sorted
// by tool name so the list is stable from one call to the next (a model's
// cached prefix depends on it). Two capabilities that derive one tool name
// are refused whole (`txco_caps_tool_names`): lint and activation refuse
// the same collision, so this is a backstop.
func capsListTools(into string, entries []capEntry, filter *capsFilter) event.Payload {
	out := capsToolsOut{Tools: []capToolOut{}}
	owner := map[string]string{}
	for _, e := range entries {
		if !filter.keep(e.Name) {
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
		tool := capdecl.ToolName(e.Name)
		if other, dup := owner[tool]; dup {
			return identityErr(into, "caps", "tool_names", fmt.Sprintf("capabilities %q and %q both become the tool %q; rename one", other, e.Name, tool))
		}
		owner[tool] = e.Name
		out.Tools = append(out.Tools, capToolOut{
			Name:        tool,
			Description: e.Decl.Description,
			Parameters:  e.Decl.InputSchema(),
			Binding:     capToolBinding{Capability: e.Name},
			Effect:      e.Decl.Effect,
		})
	}
	sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
	out.Count = len(out.Tools)
	out.Missing = filter.missing()
	return identityOK(into, out)
}
