package include

// `$[path.json]` is `&include` for structured values: the named file's
// JSON, written back into the op as a txcl literal (txcl's literals are
// JSON-shaped: `[...]`, `{"k": v}`, strings, numbers, true, false, null).
// It is expanded at the same time, by the same walk, under the same file
// rules; what differs is what gets written:
//
//   - the value is parsed and checked here, so a typo in the file fails
//     `txco apply` naming the file, line and column instead of a WHEN
//     quietly evaluating false at runtime;
//   - the literal is compact, on one line, with the file's key order, so
//     the same bytes always expand to the same text (dev fingerprints,
//     install hashes and ops_uniq all hash the expanded op);
//   - strings are re-escaped the txcl way (Quote), since JSON's `\uXXXX`
//     is not a txcl escape;
//   - numbers must be lossless. txcl has no exponent syntax and the
//     envelope carries float64, so an integer must fit in ±2^53 and a
//     non-integer must be 0 or between 1e-15 and 2^53 in magnitude; it
//     is written as the shortest plain decimal that reads back to the
//     same float64. Anything else is an error, never a silent conversion.
//
// The contents are data: a `$[...]` or `&include(...)` inside a JSON
// string stays text.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxDepth bounds nesting so a pathological file cannot stress the
// recursive writer or, later, the recursive parser.
const maxDepth = 128

// twoPow53 is the largest magnitude every integer up to which float64,
// how numbers travel in the envelope, represents exactly.
const twoPow53 = 1 << 53

// checkLiteralPath is the spelling rule for a `$[...]` path: a plain
// file name of letters, digits, '.', '_', '-' and '/', ending in `.json`
// in lower case, so a path that works on a case-insensitive filesystem
// also works on Linux. Absolute paths, escaping the stack, symlinks,
// size and encoding are read's job.
func checkLiteralPath(p string) error {
	if p == "" {
		return errors.New("empty path")
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-', c == '/':
		default:
			return fmt.Errorf("unsupported character %q in the path; use letters, digits, '.', '_', '-' and '/'", c)
		}
	}
	if !strings.HasSuffix(p, ".json") {
		return errors.New("the file must be named *.json, in lower case")
	}
	return nil
}

// jsonToTxcl writes the single JSON value in text as a one-line txcl
// literal. Errors describe the value, not the op; Expand adds the op's
// file and line.
func jsonToTxcl(text string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	w := &litWriter{dec: dec, src: text}
	tok, err := dec.Token()
	if err != nil {
		if err == io.EOF {
			return "", errors.New("empty file; expected one JSON value")
		}
		return "", w.syntax(err)
	}
	if err := w.value(tok, 0); err != nil {
		return "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return "", errors.New("trailing data after the JSON value")
		}
		return "", w.syntax(err)
	}
	return w.b.String(), nil
}

type litWriter struct {
	dec *json.Decoder
	src string
	b   strings.Builder
}

func (w *litWriter) value(tok json.Token, depth int) error {
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '[':
			return w.array(depth + 1)
		case '{':
			return w.object(depth + 1)
		}
		return fmt.Errorf("invalid JSON: unexpected %q", v)
	case string:
		return w.str(v)
	case json.Number:
		return w.number(v)
	case bool:
		if v {
			w.b.WriteString("true")
		} else {
			w.b.WriteString("false")
		}
	case nil:
		w.b.WriteString("null")
	}
	return nil
}

func (w *litWriter) array(depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("nested deeper than %d levels", maxDepth)
	}
	w.b.WriteByte('[')
	first := true
	for w.dec.More() {
		tok, err := w.dec.Token()
		if err != nil {
			return w.syntax(err)
		}
		if !first {
			w.b.WriteByte(',')
		}
		first = false
		if err := w.value(tok, depth); err != nil {
			return err
		}
	}
	if _, err := w.dec.Token(); err != nil { // the closing ]
		return w.syntax(err)
	}
	w.b.WriteByte(']')
	return nil
}

func (w *litWriter) object(depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("nested deeper than %d levels", maxDepth)
	}
	w.b.WriteByte('{')
	seen := map[string]bool{}
	first := true
	for w.dec.More() {
		ktok, err := w.dec.Token()
		if err != nil {
			return w.syntax(err)
		}
		key, _ := ktok.(string)
		if seen[key] {
			return fmt.Errorf("duplicate key %q", key)
		}
		seen[key] = true
		if !first {
			w.b.WriteByte(',')
		}
		first = false
		if err := w.str(key); err != nil {
			return err
		}
		w.b.WriteByte(':')
		vtok, err := w.dec.Token()
		if err != nil {
			return w.syntax(err)
		}
		if err := w.value(vtok, depth); err != nil {
			return err
		}
	}
	if _, err := w.dec.Token(); err != nil { // the closing }
		return w.syntax(err)
	}
	w.b.WriteByte('}')
	return nil
}

// str writes s as a txcl string. The decoder has already turned JSON
// escapes into UTF-8; two results have no txcl spelling: NUL, which the
// lexer reads as end of input, and U+FFFD standing in for a lone
// surrogate escape, which the decoder substitutes silently. The file is
// valid UTF-8 (read checked), so a U+FFFD that is not in the file's own
// bytes can only have come from such an escape.
func (w *litWriter) str(s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return errors.New(`a string contains \u0000, which a txcl string cannot hold`)
	}
	if strings.ContainsRune(s, utf8.RuneError) && !strings.ContainsRune(w.src, utf8.RuneError) {
		return errors.New(`a string contains a lone surrogate escape (\uD800-\uDFFF without its pair), which has no character to write`)
	}
	w.b.WriteString(Quote(s))
	return nil
}

// number writes n inside the lossless domain, or says why it cannot.
func (w *litWriter) number(n json.Number) error {
	raw := string(n)
	if isIntegerText(raw) {
		i, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || i > twoPow53 || i < -twoPow53 {
			return fmt.Errorf("integer %s is outside ±2^53 (9007199254740992) and would lose precision in the envelope, which carries numbers as float64", raw)
		}
		w.b.WriteString(strconv.FormatInt(i, 10)) // also writes -0 as 0
		return nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("number %s is outside float64's range", raw)
	}
	if a := math.Abs(f); a != 0 && (a < 1e-15 || a >= twoPow53) {
		return fmt.Errorf("number %s is outside the lossless range: a non-integer must be 0 or between 1e-15 and 2^53 in magnitude (txcl writes numbers as plain decimals)", raw)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0" // lexes as FLOAT; an INT this large would be clamped
	}
	if g, err := strconv.ParseFloat(s, 64); err != nil || math.Float64bits(g) != math.Float64bits(f) {
		return fmt.Errorf("number %s does not read back from its txcl spelling %s", raw, s)
	}
	w.b.WriteString(s)
	return nil
}

// isIntegerText matches -?[0-9]+ (JSON forbids leading zeros and `+`).
func isIntegerText(s string) bool {
	if strings.HasPrefix(s, "-") {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// syntax turns a decoder error into `invalid JSON at line L, column C: …`.
func (w *litWriter) syntax(err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		line, col := lineCol(w.src, se.Offset)
		return fmt.Errorf("invalid JSON at line %d, column %d: %s", line, col, se.Error())
	}
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		line, col := lineCol(w.src, int64(len(w.src)))
		return fmt.Errorf("invalid JSON at line %d, column %d: unexpected end of file", line, col)
	}
	return fmt.Errorf("invalid JSON: %w", err)
}

// lineCol places a decoder offset (bytes read when the error was seen,
// i.e. just past the offending byte) as a 1-based line and column.
func lineCol(src string, off int64) (int, int) {
	pos := int(off) - 1
	if pos < 0 {
		pos = 0
	}
	if pos > len(src) {
		pos = len(src)
	}
	head := src[:pos]
	line := 1 + strings.Count(head, "\n")
	col := pos - strings.LastIndex(head, "\n")
	return line, col
}
