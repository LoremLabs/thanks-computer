// Package include expands `&include("path")` in txcl source: the named
// file's contents, written in as a txcl string literal.
//
// Expansion runs on the CLI side, when ops are read from an OPS/ tree
// (cli/bundle's walker), so everything that deploys, compares or checks
// ops sees the expanded text and the admin API never sees the directive,
// exactly like `op://NAME`. It is textual and happens before parsing, so
// `&include("x")` works anywhere a string literal does, including inside
// `[...]` and on the right of a WHEN, where a real &function cannot go.
//
// The rules:
//   - one plain "…" argument: a path relative to the .txcl's directory;
//     `../` is fine as long as the file stays inside its stack directory
//     (OPS/<stack>/), which is what a package carries;
//   - a regular file, no symlinks anywhere below the stack directory;
//   - UTF-8 text without NUL, at most MaxBytes; a leading BOM is dropped,
//     everything else is kept byte for byte;
//   - included text is data, never scanned for further includes.
//
// The literal is written on one line (newlines as \n), so line numbers in
// the .txcl stay what the author sees.
package include

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
)

// MaxBytes caps one included file (the same cap as txco://read-file).
const MaxBytes = 1 << 20

// fn is the directive's name after the `&`.
const fn = "include"

// Ref is one `&include("path")` in a txcl source.
type Ref struct {
	Start, End int    // byte span of the whole call, `&include` through `)`
	Path       string // the argument as written
	Line       int    // 1-based line of the `&`
}

// SyntaxError is a malformed directive, or source that does not lex.
type SyntaxError struct {
	Line int
	Msg  string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

type spanTok struct {
	tok        token.Token
	start, end int
}

func lex(src string) ([]spanTok, []string) {
	l := lexer.New(src)
	var out []spanTok
	for {
		tok := l.NextToken()
		if tok.Type == token.EOF {
			return out, l.Errors()
		}
		s, e := l.Span()
		out = append(out, spanTok{tok, s, e})
	}
}

// Find returns every `&include("path")` in src, in order. A malformed one
// (no parentheses, not exactly one plain string argument) is an error, as
// is source that does not lex, since a broken string could hide one.
func Find(src string) ([]Ref, error) {
	if !strings.Contains(src, "&"+fn) {
		return nil, nil
	}
	toks, lexErrs := lex(src)
	var refs []Ref
	for i, t := range toks {
		if t.tok.Type != token.AMP_IDENT || t.tok.Literal != fn {
			continue
		}
		line := 1 + strings.Count(src[:t.start], "\n")
		if len(lexErrs) > 0 {
			return nil, &SyntaxError{line, "&include: " + lexErrs[0]}
		}
		if i+3 >= len(toks) || toks[i+1].tok.Type != token.LPAREN ||
			toks[i+2].tok.Type != token.STRING || src[toks[i+2].start] != '"' ||
			toks[i+3].tok.Type != token.RPAREN {
			return nil, &SyntaxError{line, `&include takes one quoted path: &include("file")`}
		}
		refs = append(refs, Ref{Start: t.start, End: toks[i+3].end, Path: toks[i+2].tok.Literal, Line: line})
	}
	return refs, nil
}

// Has reports whether src still contains an &include directive: the
// admin API's guard against an op uploaded without expansion. Token-based,
// so text that merely mentions `&include(` inside a string (an expanded
// include, say) does not count.
func Has(src string) bool {
	if !strings.Contains(src, "&"+fn) {
		return false
	}
	toks, _ := lex(src)
	for _, t := range toks {
		if t.tok.Type == token.AMP_IDENT && t.tok.Literal == fn {
			return true
		}
	}
	return false
}

// Quote writes s as a txcl string literal that lexes back to exactly s.
// Hand-rolled on purpose: txcl knows only \" \\ \n \r \t, and keeps any
// other backslash sequence (strconv.Quote's \x01, json's \u003c) as
// literal text. Newlines are escaped so the literal stays on one line.
// s must not contain NUL, which the lexer reads as end of input.
func Quote(s string) string {
	var b strings.Builder
	b.Grow(len(s) + len(s)/16 + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Expand replaces each `&include("path")` in src, the .txcl at txclPath,
// with the file's contents as a string literal. Paths are fsys paths;
// stackDir is the op's stack directory (e.g. "OPS/ingest"), which every
// included file must stay inside. deps lists the files read, once each.
// Errors read `<txclPath>:<line>: &include("<path>"): <reason>`.
func Expand(fsys fs.FS, stackDir, txclPath, src string) (out string, deps []string, err error) {
	refs, err := Find(src)
	if err != nil {
		var se *SyntaxError
		if errors.As(err, &se) {
			return "", nil, fmt.Errorf("%s:%d: %s", txclPath, se.Line, se.Msg)
		}
		return "", nil, fmt.Errorf("%s: %w", txclPath, err)
	}
	if len(refs) == 0 {
		return src, nil, nil
	}
	var b strings.Builder
	last := 0
	seen := map[string]bool{}
	for _, r := range refs {
		p, text, err := read(fsys, stackDir, path.Dir(txclPath), r.Path)
		if err != nil {
			return "", nil, fmt.Errorf("%s:%d: &include(%q): %w", txclPath, r.Line, r.Path, err)
		}
		b.WriteString(src[last:r.Start])
		b.WriteString(Quote(text))
		last = r.End
		if !seen[p] {
			seen[p] = true
			deps = append(deps, p)
		}
	}
	b.WriteString(src[last:])
	return b.String(), deps, nil
}

var bom = []byte("\xef\xbb\xbf")

// read resolves rel against dir, confines it to stackDir and returns the
// file's text.
func read(fsys fs.FS, stackDir, dir, rel string) (string, string, error) {
	if rel == "" {
		return "", "", errors.New("empty path")
	}
	if path.IsAbs(rel) {
		return "", "", errors.New("absolute path; name the file relative to this .txcl")
	}
	p := path.Join(dir, rel)
	if !strings.HasPrefix(p, stackDir+"/") {
		return "", "", fmt.Errorf("resolves outside the stack directory %s/; included files travel with their stack, so keep them inside it", stackDir)
	}
	// Refuse a symlink at any level below the stack directory: packages
	// and asset uploads drop symlinks, so what applied here would not
	// travel, and a link could pull in any file on this machine.
	segs := strings.Split(strings.TrimPrefix(p, stackDir+"/"), "/")
	cur := stackDir
	var fi fs.FileInfo
	for _, seg := range segs {
		cur = path.Join(cur, seg)
		var err error
		fi, err = fs.Lstat(fsys, cur)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return "", "", fmt.Errorf("no such file %s", p)
			}
			return "", "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return "", "", fmt.Errorf("%s is a symlink; include the file itself", cur)
		}
	}
	if !fi.Mode().IsRegular() {
		return "", "", fmt.Errorf("%s is not a regular file", p)
	}
	if fi.Size() > MaxBytes {
		return "", "", fmt.Errorf("%s is %d bytes; the limit is %d", p, fi.Size(), MaxBytes)
	}
	raw, err := fs.ReadFile(fsys, p)
	if err != nil {
		return "", "", err
	}
	if len(raw) > MaxBytes {
		return "", "", fmt.Errorf("%s is %d bytes; the limit is %d", p, len(raw), MaxBytes)
	}
	raw = bytes.TrimPrefix(raw, bom)
	if !utf8.Valid(raw) {
		return "", "", fmt.Errorf("%s is not UTF-8 text", p)
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", "", fmt.Errorf("%s contains a NUL byte, which a txcl string cannot hold", p)
	}
	return p, string(raw), nil
}
