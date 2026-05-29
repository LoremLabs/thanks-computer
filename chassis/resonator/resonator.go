package resonator

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
)

type MatchType string
type PhraseType string

const (
	LT     MatchType = "lt"
	LT_EQ  MatchType = "lteq"
	GT     MatchType = "gt"
	GT_EQ  MatchType = "gteq"
	EQ     MatchType = "eq"
	NOT_EQ MatchType = "ne"
)

type Branch struct {
	Path string `json:"path,omitempty"`
}

type BranchValue struct {
	Path  string    `json:"path,omitempty"`  // .branch.subbranch
	Value ast.Value `json:"value,omitempty"` // ast.Literal{V: 42} today; ast.PathRef / ast.FunctionCall reserved
}

type Condition struct {
	Star       bool         `json:"isStar,omitempty"`     // true = matches everything
	Branch     *Branch      `json:"branch,omitempty"`     // .branch.subbranch
	MatchType  MatchType    `json:"matchType,omitempty"`  // ==
	MatchValue interface{}  `json:"matchValue,omitempty"` // 5
	Adds       *BranchValue `json:"adds,omitempty"`       // for select, adding branches
	Prunes     *BranchValue `json:"prunes,omitempty"`     // for select, removing branches
}

// WhenExpr is the boolean expression tree for a WHEN clause. Exactly
// one of {And, Or, Not, HasLeaf, Star} is set on each node.
//
// And/Or are n-ary (slices of values), Not is unary and uses a pointer
// for self-recursion (Go can't embed a value-type into itself). Leaf is
// held by value with HasLeaf as the discriminator so the tree contains
// no leaf-level pointers and there's no aliasing back into any source
// slice.
type WhenExpr struct {
	And     []WhenExpr `json:"and,omitempty"`
	Or      []WhenExpr `json:"or,omitempty"`
	Not     *WhenExpr  `json:"not,omitempty"`
	Leaf    Condition  `json:"leaf,omitempty"`
	HasLeaf bool       `json:"has_leaf,omitempty"`
	Star    bool       `json:"star,omitempty"`
}

type When struct {
	// Expr is the canonical tree shape produced by the parser. New rules
	// always populate Expr.
	Expr *WhenExpr `json:"expr,omitempty"`
	// Conditions is the legacy flat-AND shape. Programmatic callers (and
	// any in-flight JSON written before the grammar v2 upgrade) populate
	// this; WhenMatches promotes it to an Expr at evaluation time.
	Conditions []Condition `json:"conditions,omitempty"`
}

type Set struct {
	Overrides []BranchValue `json:"overrides,omitempty"`
}

// SelectAssignment is one `<source> AS <dest> [DEFAULT <literal>]`
// clause inside a SELECT. From and To are envelope paths (with the
// leading `.` or `@` sugar accepted by the parser, normalized for
// gjson/sjson at evaluation time). Default is the literal used when
// From resolves to a missing path or the empty string.
type SelectAssignment struct {
	From       string    `json:"from"`
	To         string    `json:"to"`
	Default    ast.Value `json:"default,omitempty"`
	HasDefault bool      `json:"has_default,omitempty"`
}

// Select copies envelope values from path → path with optional literal
// fallback. Assignments run before the rule's EXEC (so the EXEC sees
// the copied values on its input view) and are also overlaid onto the
// rule's response so the writes persist even when the rule has no
// EXEC. Distinct from SET (literal RHS only, op-scoped, requires EXEC
// to commit) and EMIT (literal RHS only, post-EXEC overlay).
type Select struct {
	Assignments []SelectAssignment `json:"assignments,omitempty"`
}

// Resonator is the parsed form of one txcl rule. Built by the parser,
// consumed by the processor at match-and-dispatch time.
type Resonator struct {
	When     *When                  `json:"when,omitempty"`
	Select   *Select                `json:"select,omitempty"`
	SetPre   *Set                   `json:"setPre,omitempty"`  // set before select
	SetPost  *Set                   `json:"setPost,omitempty"` // set after select
	With     map[string]ast.Value   `json:"with,omitempty"`
	Priority int64                  `json:"priority"`
	Exec     string                 `json:"exec,omitempty"`
	// Emit overlays values onto THIS rule's response after EXEC,
	// before the per-scope merge. Overwrite semantics (not the
	// set-if-absent behavior of SET POST). Lets a rule contribute
	// literal fields to the merged scope output without needing a
	// real HTTP/handler call: pair with EXEC for "enrich the
	// response", or write EMIT alone for a "synthetic emitter".
	Emit *Set `json:"emit,omitempty"`
}

const (
	WHEN     PhraseType = "WHEN"
	SETPRE   PhraseType = "SETPRE"
	SELECT   PhraseType = "SELECT"
	SETPOST  PhraseType = "SETPOST"
	WITH     PhraseType = "WITH"
	PRIORITY PhraseType = "PRIORITY"
	EXEC     PhraseType = "EXEC"
	EMIT     PhraseType = "EMIT"
)

type Phrase struct {
	Type     PhraseType
	When     *When                  `json:"when,omitempty"`
	Select   *Select                `json:"select,omitempty"`
	SetPre   *Set                   `json:"setPre,omitempty"`  // set before select
	SetPost  *Set                   `json:"setPost,omitempty"` // set after select
	With     map[string]ast.Value   `json:"with,omitempty"`
	Priority int64                  `json:"priority"`
	Exec     string                 `json:"exec,omitempty"`
	Emit     *Set                   `json:"emit,omitempty"`
}

func New() *Resonator {
	r := &Resonator{}
	return r
}

// WhenMatches returns true if the WHEN clause matches the input
// envelope. Semantics preserved across the grammar v2 upgrade:
//   - empty input never matches (caller contract)
//   - nil/empty When matches everything (star)
//   - legacy When{Conditions: [...]} is promoted to an Expr tree on
//     the fly; any Star condition in the list short-circuits to match
//   - new When{Expr: ...} is walked directly
func (res Resonator) WhenMatches(input string) bool {

	if input == "" {
		// no input, no match
		return false
	}

	if res.When == nil {
		// no when, star match
		return true
	}

	expr := res.When.Expr
	if expr == nil {
		// Legacy shape: promote Conditions → Expr for this evaluation.
		// (We don't write back into res.When because the receiver is by
		// value and the When pointer is potentially shared across
		// goroutines.)
		if len(res.When.Conditions) == 0 {
			return true
		}
		// Preserve legacy "any Star in the list matches everything"
		// semantics. In practice the parser only emits Star when WHEN
		// is `WHEN *`, but programmatic callers may construct mixed
		// shapes.
		for _, c := range res.When.Conditions {
			if c.Star {
				return true
			}
		}
		leaves := make([]WhenExpr, 0, len(res.When.Conditions))
		for _, c := range res.When.Conditions {
			leaves = append(leaves, WhenExpr{Leaf: c, HasLeaf: true})
		}
		expr = &WhenExpr{And: leaves}
	}

	return evalExpr(expr, input)
}

// evalExpr walks the WhenExpr tree. Short-circuit semantics fall out
// of the early returns; the And and Or loops iterate by index to keep
// each WhenExpr addressable without aliasing through a range variable.
func evalExpr(e *WhenExpr, input string) bool {
	switch {
	case e == nil, e.Star:
		return true
	case e.HasLeaf:
		return evalLeaf(&e.Leaf, input)
	case len(e.And) > 0:
		for i := range e.And {
			if !evalExpr(&e.And[i], input) {
				return false
			}
		}
		return true
	case len(e.Or) > 0:
		for i := range e.Or {
			if evalExpr(&e.Or[i], input) {
				return true
			}
		}
		return false
	case e.Not != nil:
		return !evalExpr(e.Not, input)
	}
	return false
}

// evalLeaf runs a single Condition against the input. The big switch
// on MatchType is the original WhenMatches body, lifted unchanged.
func evalLeaf(cond *Condition, input string) bool {
	if cond.Star {
		return true
	}
	if cond.Branch == nil {
		return false
	}
	branch := cond.Branch.Path
	if len(branch) == 0 {
		return false
	}
	if len(cond.MatchType) == 0 {
		return false
	}
	branch = strings.TrimPrefix(branch, ".")
	val := gjson.Get(input, branch)

	switch cond.MatchType {
	case "=~":
		re, err := regexp.Compile(cond.MatchValue.(string))
		if err == nil {
			return re.MatchString(val.String())
		}
	case "!~":
		re, err := regexp.Compile(cond.MatchValue.(string))
		if err == nil {
			return !re.MatchString(val.String())
		}
	case "eq":
		switch v := cond.MatchValue.(type) {
		case int64:
			return val.Int() == v
		case float64:
			return val.Float() == v
		case string:
			return val.String() == v
		case bool:
			return val.Bool() == v
		}
	case "ne":
		switch v := cond.MatchValue.(type) {
		case int64:
			return val.Int() != v
		case float64:
			return val.Float() != v
		case string:
			return val.String() != v
		case bool:
			return val.Bool() != v
		}
	case "lt":
		switch v := cond.MatchValue.(type) {
		case int64:
			return val.Int() < v
		case float64:
			return val.Float() < v
		case string:
			return val.String() < v
		}
	case "gt":
		switch v := cond.MatchValue.(type) {
		case int64:
			return val.Int() > v
		case float64:
			return val.Float() > v
		case string:
			return val.String() > v
		}
	case "gteq":
		switch v := cond.MatchValue.(type) {
		case int64:
			return val.Int() >= v
		case float64:
			return val.Float() >= v
		case string:
			return val.String() >= v
		}
	case "lteq":
		switch v := cond.MatchValue.(type) {
		case int64:
			return val.Int() <= v
		case float64:
			return val.Float() <= v
		case string:
			return val.String() <= v
		}
	}
	return false
}
