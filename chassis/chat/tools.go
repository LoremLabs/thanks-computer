package chat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/jsonschemax"
)

// Tool is one model-facing tool definition: what a model is offered, in the
// chassis's neutral shape. A backend translates it to its provider's wire
// form; the handler reads Name, Description and Parameters and carries
// Binding through untouched.
//
// Where definitions come from is not this package's concern: the catalogue
// derives one from a capability's declaration (`txco://caps/list WITH format
// = "tools"`), an executor writes one for a tool it discovered elsewhere (an
// MCP server's, an OpenAPI operation). `ai://chat` never resolves a name
// against the catalogue: a string where a definition belongs is an error.
type Tool struct {
	// Name is what the model calls. It must already be legal on every
	// provider this dialect family serves: letters, digits, `_` and `-`, at
	// most 64. The chassis refuses any other name rather than mapping it;
	// the catalogue derives a legal one (`web.search` → `web_search`).
	Name string `json:"name"`
	// Description is what the model reads to decide whether to call.
	Description string `json:"description,omitempty"`
	// Parameters is the JSON Schema of the arguments: an object schema.
	Parameters json.RawMessage `json:"parameters"`
	// Binding is the executor's own note of how to run a call to this tool
	// (`{capability, input, arguments_into}` in OnePony's use). The chassis
	// never reads it and never sends it to a provider; it copies it onto
	// each returned call to the matching offered tool, so one dispatch rule
	// needs no lookup. It is request metadata, never model output.
	Binding json.RawMessage `json:"binding,omitempty"`
}

// ToolCall is one call a model made, in the neutral shape.
type ToolCall struct {
	// ID is the provider's id for the call; a result refers to it.
	ID string `json:"id"`
	// Name is the tool's name as offered (never a provider's wire name).
	Name string `json:"name"`
	// Arguments is the call's arguments when they parsed as a JSON object,
	// else null (and Raw holds what the model wrote). Never omitted, so an
	// executor can test it.
	Arguments json.RawMessage `json:"arguments"`
	// Raw is the model's argument text when Arguments could not be parsed
	// as an object, kept so the transcript can be sent back as it was.
	Raw string `json:"raw,omitempty"`
	// Problem is set by the handler when the call's transport shape is
	// wrong: not_offered, not_json, truncated or duplicate_id. A call with
	// a problem must not run.
	Problem *ToolProblem `json:"problem,omitempty"`
	// Binding is copied by the handler from the offered Tool of the same
	// request whose Name matches. Anything a provider returned under this
	// key was dropped at the wire.
	Binding json.RawMessage `json:"binding,omitempty"`
}

// ToolProblem says why a returned call cannot run as it is.
type ToolProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// The codes a ToolProblem carries.
const (
	// ProblemNotOffered: the call names a tool this request did not offer.
	ProblemNotOffered = "not_offered"
	// ProblemNotJSON: the arguments are not a JSON object; Raw has them.
	ProblemNotJSON = "not_json"
	// ProblemTruncated: the answer hit the output limit, so the arguments
	// of every call in it may be cut off.
	ProblemTruncated = "truncated"
	// ProblemDuplicateID: the id repeats one earlier in the same answer.
	ProblemDuplicateID = "duplicate_id"
)

// How an answer ended, in the neutral vocabulary. A backend maps its
// provider's words onto these; an empty Finish means the provider said
// nothing.
const (
	FinishStop      = "stop"
	FinishLength    = "length"
	FinishToolCalls = "tool_calls"
	FinishFiltered  = "filtered"
	FinishOther     = "other"
)

// The tool_choice values a request may carry; any other value names one
// offered tool to force.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
)

// The strict modes a request may carry.
const (
	StrictOff     = "off"
	StrictPrefer  = "prefer"
	StrictRequire = "require"
)

// ToolFeatures are the fixed facts a backend states about tool calling:
// whether it takes tools at all, whether it can control parallel calls,
// whether it can decode strictly. Fixed per backend, not per model: the
// chassis translates what a backend guarantees and refuses the rest.
type ToolFeatures struct {
	Tools    bool
	Parallel bool
	Strict   bool
}

// ToolCapable is the optional interface a backend implements to take
// tools. It is checked by type assertion, so a backend that does not
// implement it (a test double, an older backend) keeps compiling and is
// simply refused a request with tools.
type ToolCapable interface {
	ToolFeatures() ToolFeatures
}

// ToolNameRE is the grammar of a tool's name: what OpenAI and Anthropic
// both accept.
var ToolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidToolName reports whether s may name a tool.
func ValidToolName(s string) bool { return ToolNameRE.MatchString(s) }

// ValidateTools checks a request's definitions: a legal name, parameters
// that are an object schema and compile, a binding that is an object when
// present, and no two definitions with one name. The error is a
// *ToolRequestError of kind tool_invalid or tool_names.
func ValidateTools(tools []Tool) error {
	seen := make(map[string]bool, len(tools))
	for i, t := range tools {
		at := fmt.Sprintf("tools[%d]", i)
		if t.Name != "" {
			at = fmt.Sprintf("tools[%d] (%s)", i, t.Name)
		}
		if !ValidToolName(t.Name) {
			return &ToolRequestError{Kind: KindToolInvalid, Reason: at + ": a tool's name is letters, digits, _ and -, at most 64; the catalogue derives one from a capability name"}
		}
		if seen[t.Name] {
			return &ToolRequestError{Kind: KindToolNames, Reason: at + ": two definitions share this name"}
		}
		seen[t.Name] = true
		if len(t.Parameters) == 0 {
			return &ToolRequestError{Kind: KindToolInvalid, Reason: at + ": parameters (a JSON Schema object) is required"}
		}
		var top struct {
			Type json.RawMessage `json:"type"`
		}
		if err := json.Unmarshal(t.Parameters, &top); err != nil || !isObjectJSON(t.Parameters) {
			return &ToolRequestError{Kind: KindToolInvalid, Reason: at + ": parameters must be a JSON object"}
		}
		if string(top.Type) != `"object"` {
			return &ToolRequestError{Kind: KindToolInvalid, Reason: at + `: parameters must be an object schema ("type": "object")`}
		}
		if _, err := jsonschemax.Compile(t.Parameters); err != nil {
			return &ToolRequestError{Kind: KindToolInvalid, Reason: at + ": parameters: " + err.Error()}
		}
		if len(t.Binding) > 0 && !isObjectJSON(t.Binding) {
			return &ToolRequestError{Kind: KindToolInvalid, Reason: at + ": binding must be a JSON object"}
		}
	}
	return nil
}

// isObjectJSON reports whether raw is a JSON object (not null, not an array).
func isObjectJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return strings.HasPrefix(s, "{") && json.Valid(raw)
}

// The kinds of ToolRequestError; each is the suffix of its code.
const (
	KindToolInvalid       = "tool_invalid"
	KindToolNames         = "tool_names"
	KindToolsUnsupported  = "tools_unsupported"
	KindStrictUnsupported = "strict_unsupported"
	KindPendingToolResult = "pending_tool_result"
)

// ToolRequestError is every tool-related refusal of a chat request. Its
// code is `txco_chat_<Kind>`. IDs, when set, name the tool calls the
// refusal is about (the unanswered ones of a pending_tool_result) and are
// surfaced as `chat.error.detail.ids`.
type ToolRequestError struct {
	Kind   string
	Reason string
	IDs    []string
}

func (e *ToolRequestError) Error() string { return "chat: " + e.Kind + ": " + e.Reason }
func (e *ToolRequestError) Code() string  { return "txco_chat_" + e.Kind }
func (e *ToolRequestError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{"code": e.Code(), "message": e.Error()})
	return string(b)
}

// ErrorDetail is what `chat.error.detail` carries: the ids, when any.
func (e *ToolRequestError) ErrorDetail() map[string]any {
	if len(e.IDs) == 0 {
		return nil
	}
	return map[string]any{"ids": e.IDs}
}

// DetailedError is the optional interface an error implements to add a
// `detail` object beside `code` and `message` in `chat.error`. Errors that
// do not implement it keep the two-key shape they have today.
type DetailedError interface {
	ErrorDetail() map[string]any
}
