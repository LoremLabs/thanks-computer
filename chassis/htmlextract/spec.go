// Package htmlextract picks values out of an HTML document with CSS
// selectors, within fixed bounds. It does no networking: callers hand it
// a body they fetched (txco://html/extract uses chassis/fetch).
//
// The document is untrusted, and selector matching can't be interrupted
// once it starts, so the work is bounded up front instead: the selector
// grammar is limited to features whose cost per element is constant or
// one walk up the tree, and the document's tokens, nodes, depth and
// attributes are capped, with an estimate of the total matching work
// checked before any matching runs.
package htmlextract

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/andybalholm/cascadia"
	"github.com/tidwall/gjson"
)

// Error codes.
const (
	CodeInvalidSelectors   = "invalid_selectors"
	CodeParseFailed        = "parse_failed"
	CodeDocumentTooComplex = "document_too_complex"
	CodeOutputLimit        = "output_limit_exceeded"
	CodeTimeout            = "timeout"
)

// Error is a refused spec or document. Field names the selector field a
// spec error is about.
type Error struct {
	Code    string
	Message string
	Field   string
}

func (e *Error) Error() string { return "htmlextract: " + e.Code + ": " + e.Message }

func specErr(field, format string, args ...any) *Error {
	msg := fmt.Sprintf(format, args...)
	if field != "" {
		msg = fmt.Sprintf("selectors.%s: %s", field, msg)
	}
	return &Error{Code: CodeInvalidSelectors, Message: msg, Field: field}
}

// Limits bound one extraction. The zero value is not usable; start from
// DefaultLimits.
type Limits struct {
	MaxFields        int   // selector fields per call
	MaxSelectorBytes int   // one selector's length
	MaxMatches       int   // a field's `limit` ceiling
	DefaultMatches   int   // a field's `limit` when `all` is set without one
	MaxValueBytes    int   // one value; longer ones are cut (truncated)
	MaxOutputBytes   int   // the whole data object; more fails
	MaxTokens        int   // tokens in the document
	MaxNodes         int   // nodes in the parsed tree
	MaxBuildNodes    int   // the prescan's estimate of nodes and attributes the parser will create
	MaxDepth         int   // nesting of the parsed tree
	MaxAttrs         int   // attributes on one element
	MaxWork          int64 // estimated selector work: nodes × per-field weight
	MaxTextVisits    int   // nodes walked collecting text, across all fields
	// MaxTime is the CPU budget for parsing and matching; a document that
	// takes longer is document_too_complex. 0 means only ctx bounds it.
	MaxTime time.Duration
}

// DefaultLimits are the chassis ceilings, sized for the 3 MiB fetch cap:
// a dense 3 MiB page of ordinary markup (about 530k tokens, 380k nodes)
// fits, and costs about 80 MB and a quarter of a second to parse and
// match (bench_test.go). Callers may lower them; the handler never
// raises them.
func DefaultLimits() Limits {
	return Limits{
		MaxFields:        20,
		MaxSelectorBytes: 512,
		MaxMatches:       50,
		DefaultMatches:   10,
		MaxValueBytes:    4 << 10,
		MaxOutputBytes:   64 << 10,
		MaxTokens:        600_000,
		MaxNodes:         400_000,
		MaxBuildNodes:    1_000_000,
		MaxDepth:         512,
		MaxAttrs:         256,
		MaxWork:          200_000_000,
		MaxTextVisits:    1_000_000,
		MaxTime:          time.Second,
	}
}

// Spec is one named field to extract.
type Spec struct {
	Name     string
	Selector string
	Text     bool   // the element's text
	Attr     string // or one attribute's value
	All      bool   // every match (up to Limit), or the first
	Limit    int

	group cascadia.SelectorGroup
	alts  int // complex selectors in the group
	desc  int // descendant combinators in the group (each walks up the tree)
}

var fieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

// ParseSpecs reads the `selectors` object: field name → {selector, text |
// attr, all?, limit?}. Field order is kept. Every problem is an
// invalid_selectors error naming the field.
func ParseSpecs(raw string, lim Limits) ([]Spec, error) {
	obj := gjson.Parse(raw)
	if !obj.IsObject() {
		return nil, specErr("", "`selectors` must be an object of named fields, each {selector, text: true | attr}")
	}
	var specs []Spec
	seen := map[string]bool{}
	var err *Error
	obj.ForEach(func(k, v gjson.Result) bool {
		name := k.String()
		if len(specs) >= lim.MaxFields {
			err = specErr("", "at most %d fields", lim.MaxFields)
			return false
		}
		if !fieldName.MatchString(name) {
			err = specErr("", "field name %q must be letters, digits, `_` and `-`, starting with a letter or `_`, at most 64 characters", truncName(name))
			return false
		}
		if seen[name] {
			err = specErr(name, "the field is named twice")
			return false
		}
		seen[name] = true
		var s Spec
		s, err = parseSpec(name, v, lim)
		if err != nil {
			return false
		}
		specs = append(specs, s)
		return true
	})
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return nil, specErr("", "name at least one field")
	}
	return specs, nil
}

func truncName(s string) string {
	if len(s) > 64 {
		return s[:64] + "…"
	}
	return s
}

func parseSpec(name string, v gjson.Result, lim Limits) (Spec, *Error) {
	s := Spec{Name: name}
	if !v.IsObject() {
		return s, specErr(name, "must be an object: {selector, text: true | attr}")
	}
	var err *Error
	v.ForEach(func(k, val gjson.Result) bool {
		switch k.String() {
		case "selector":
			if val.Type != gjson.String {
				err = specErr(name, "`selector` must be a string")
			}
			s.Selector = val.String()
		case "text":
			if val.Type != gjson.True && val.Type != gjson.False {
				err = specErr(name, "`text` must be true or false")
			}
			s.Text = val.Type == gjson.True
		case "attr":
			if val.Type != gjson.String {
				err = specErr(name, "`attr` must be a string")
			}
			s.Attr = strings.ToLower(strings.TrimSpace(val.String()))
		case "all":
			if val.Type != gjson.True && val.Type != gjson.False {
				err = specErr(name, "`all` must be true or false")
			}
			s.All = val.Type == gjson.True
		case "limit":
			n := val.Int()
			if val.Type != gjson.Number || float64(n) != val.Float() || n < 1 || n > int64(lim.MaxMatches) {
				err = specErr(name, "`limit` must be a whole number from 1 to %d", lim.MaxMatches)
			}
			s.Limit = int(n)
		default:
			err = specErr(name, "unknown key %q (selector, text, attr, all, limit)", truncName(k.String()))
		}
		return err == nil
	})
	if err != nil {
		return s, err
	}
	if s.Selector == "" {
		return s, specErr(name, "`selector` is required")
	}
	if len(s.Selector) > lim.MaxSelectorBytes {
		return s, specErr(name, "the selector is longer than %d bytes", lim.MaxSelectorBytes)
	}
	if s.Text == (s.Attr != "") {
		return s, specErr(name, "set exactly one of `text: true` or `attr`")
	}
	if len(s.Attr) > 128 {
		return s, specErr(name, "`attr` is longer than 128 bytes")
	}
	if !s.All {
		s.Limit = 1
	} else if s.Limit == 0 {
		s.Limit = lim.DefaultMatches
	}
	group, perr := cascadia.ParseGroup(s.Selector)
	if perr != nil {
		return s, specErr(name, "the selector does not parse: %s", perr.Error())
	}
	s.group = group
	for _, sel := range group {
		d, cerr := checkSel(reflect.ValueOf(sel))
		if cerr != "" {
			return s, specErr(name, "%s", cerr)
		}
		if d > 1 {
			return s, specErr(name, "use at most one descendant combinator (a space) per selector; use `>` for the others")
		}
		s.alts++
		s.desc += d
	}
	return s, nil
}

// checkSel walks a parsed selector (cascadia's unexported types, read by
// reflection) and returns how many descendant combinators it holds, or
// why it is refused. Only features whose per-element cost is constant —
// or, for the one descendant combinator allowed, a walk up the tree —
// pass. Anything unknown is refused, so a cascadia upgrade that adds a
// selector type fails closed (and this package's tests say which).
func checkSel(v reflect.Value) (int, string) {
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return 0, ""
		}
		v = v.Elem()
	}
	switch v.Type().Name() {
	case "tagSelector", "idSelector", "classSelector", "rootPseudoClassSelector", "neverMatchSelector":
		return 0, ""
	case "attrSelector":
		if v.FieldByName("operation").String() == "#=" {
			return 0, "regular-expression attribute matching (`#=`) is not supported"
		}
		return 0, ""
	case "nthPseudoClassSelector":
		// :first-child and :last-child look at one sibling; the rest
		// count siblings for every element.
		if v.FieldByName("a").Int() == 0 && v.FieldByName("b").Int() == 1 && !v.FieldByName("ofType").Bool() {
			return 0, ""
		}
		return 0, "only :first-child and :last-child are supported, not :nth-* or :*-of-type"
	case "relativePseudoClassSelector":
		name := v.FieldByName("name").String()
		if name != "not" {
			return 0, fmt.Sprintf(":%s() is not supported", name)
		}
		group := v.FieldByName("match")
		total := 0
		for i := 0; i < group.Len(); i++ {
			d, err := checkSel(group.Index(i))
			if err != "" {
				return 0, err
			}
			total += d
		}
		return total, ""
	case "compoundSelector":
		sels := v.FieldByName("selectors")
		total := 0
		for i := 0; i < sels.Len(); i++ {
			d, err := checkSel(sels.Index(i))
			if err != "" {
				return 0, err
			}
			total += d
		}
		return total, ""
	case "combinedSelector":
		total := 0
		switch byte(v.FieldByName("combinator").Uint()) {
		case ' ':
			total = 1
		case '>', '+', 0:
		case '~':
			return 0, "the general sibling combinator (`~`) is not supported"
		default:
			return 0, "an unsupported combinator"
		}
		for _, f := range []string{"first", "second"} {
			d, err := checkSel(v.FieldByName(f))
			if err != "" {
				return 0, err
			}
			total += d
		}
		return total, ""
	case "containsPseudoClassSelector":
		return 0, ":contains() and :containsOwn() are not supported"
	case "regexpPseudoClassSelector":
		return 0, ":matches() and :matchesOwn() are not supported"
	case "onlyChildPseudoClassSelector":
		return 0, ":only-child and :only-of-type are not supported"
	}
	return 0, "a selector feature this op does not support (supported: type, #id, .class, [attr] selectors, `>`, `+`, one descendant space, :not(), :first-child, :last-child, :root)"
}
