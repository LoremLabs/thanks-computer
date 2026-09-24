package outlet

import (
	"fmt"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
)

// CheckOp is the apply-time check of one EXEC "outlet://..." op against the
// stack's declarations. It runs client-side in `txco apply` and `txco lint`
// for fast feedback and server-side on validate/activate as the authority.
// Anything knowable from stack source fails here; anything that depends on
// the world at call time (a missing secret, a database that is down) comes
// back as failure data at run time instead.
//
// The statement checks are lint, not a boundary: they catch an op pointed at
// the wrong verb while the author is writing it. What actually keeps the
// call safe is bound parameters, the READ ONLY transaction behind `query`,
// the extended protocol (one statement per call) and the database role
// behind the DSN.
func CheckOp(exec string, with map[string]ast.Value, decls map[string]*Decl) error {
	name, op, err := ParseRef(exec)
	if err != nil {
		return err
	}
	decl, ok := decls[name]
	if !ok {
		if len(decls) == 0 {
			return fmt.Errorf("outlet %q is not declared: add %s in this stack", name, DeclPath(name))
		}
		return fmt.Errorf("outlet %q is not declared (declared: %v)", name, DeclNames(decls))
	}
	if op == OpExec && !decl.Writable() {
		return fmt.Errorf("outlet %q is declared `access: read`; outlet://%s/exec is refused (declare `access: write` to allow it)", name, name)
	}
	raw, ok := with["sql"]
	if !ok {
		return fmt.Errorf("outlet://%s/%s needs WITH sql = \"...\"", name, op)
	}
	sql, err := literalSQL(raw)
	if err != nil {
		return err
	}
	return CheckSQL(sql, op)
}

// literalSQL insists the statement is written in the op. That is the
// property worth keeping: the program a stack runs against an external
// system is readable in its source, diffable and auditable without
// replaying a run. Values reach the statement only through `args`.
func literalSQL(v ast.Value) (string, error) {
	switch n := v.(type) {
	case ast.Literal:
		s, ok := n.V.(string)
		if !ok {
			return "", fmt.Errorf("WITH sql must be a string literal")
		}
		return s, nil
	case ast.PathRef:
		return "", fmt.Errorf("WITH sql must be a string literal, not a path reference (@%s): write the statement in the op and bind values with args", strings.TrimPrefix(n.Path, "_txc."))
	case ast.FunctionCall:
		return "", fmt.Errorf("WITH sql must be a string literal, not &%s(...): write the statement in the op and bind values with args", n.Name)
	}
	return "", fmt.Errorf("WITH sql must be a string literal")
}

// Leading keywords per operation. `query` may only read; `exec` may also
// mutate rows. DDL and bulk verbs are named explicitly so the refusal says
// why: an outlet reads and writes rows in a schema someone else owns.
var (
	readKeywords  = map[string]bool{"SELECT": true, "WITH": true, "VALUES": true, "TABLE": true}
	writeKeywords = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": true}
	ddlKeywords   = map[string]bool{
		"CREATE": true, "ALTER": true, "DROP": true, "GRANT": true, "REVOKE": true,
		"TRUNCATE": true, "COPY": true, "VACUUM": true, "REINDEX": true, "CLUSTER": true,
	}
)

// CheckSQL lints one statement for an operation: non-empty, one statement,
// and a leading keyword that matches the verb. Comments and string literals
// are skipped, so a `;` inside a quoted value is not a second statement.
func CheckSQL(sql, op string) error {
	body, stmts := scanStatement(sql)
	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("WITH sql is empty")
	}
	if stmts > 1 {
		return fmt.Errorf("WITH sql contains more than one statement; an outlet runs exactly one per call")
	}
	kw := leadingKeyword(body)
	switch {
	case ddlKeywords[kw]:
		return fmt.Errorf("WITH sql starts with %s: DDL and bulk verbs are not allowed through an outlet (it reads and writes rows in a schema someone else owns)", kw)
	case readKeywords[kw]:
		return nil
	case writeKeywords[kw]:
		if op != OpExec {
			return fmt.Errorf("WITH sql starts with %s but the operation is %s; use outlet://<name>/exec for a statement that may change rows", kw, op)
		}
		return nil
	}
	if op == OpExec {
		return fmt.Errorf("WITH sql must start with SELECT, WITH, INSERT, UPDATE, DELETE or MERGE (got %q)", kw)
	}
	return fmt.Errorf("WITH sql must start with SELECT or WITH for a query (got %q)", kw)
}

// scanStatement returns the statement with comments blanked and the number
// of statements it holds, counting `;` only outside comments, quoted
// strings, quoted identifiers and dollar-quoted strings. A trailing `;` is
// tolerated.
func scanStatement(sql string) (string, int) {
	var out strings.Builder
	out.Grow(len(sql))
	stmts := 1
	sawText := false
	afterSemi := false
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
			continue
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			depth := 1
			i += 2
			for i < len(sql) && depth > 0 {
				if sql[i] == '/' && i+1 < len(sql) && sql[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if sql[i] == '*' && i+1 < len(sql) && sql[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
			out.WriteByte(' ')
			continue
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(sql) {
				if sql[j] == c {
					if j+1 < len(sql) && sql[j+1] == c { // doubled quote escapes
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j < len(sql) {
				j++
			}
			out.WriteString(sql[i:j])
			i = j
			sawText, afterSemi = true, false
			continue
		case c == '$':
			// $$...$$ or $tag$...$tag$
			j := i + 1
			for j < len(sql) && (sql[j] == '_' || isAlnum(sql[j])) {
				j++
			}
			if j < len(sql) && sql[j] == '$' && (j == i+1 || !isDigit(sql[i+1])) {
				tag := sql[i : j+1]
				end := strings.Index(sql[j+1:], tag)
				if end < 0 {
					out.WriteString(sql[i:])
					i = len(sql)
				} else {
					stop := j + 1 + end + len(tag)
					out.WriteString(sql[i:stop])
					i = stop
				}
				sawText, afterSemi = true, false
				continue
			}
		case c == ';':
			if sawText {
				afterSemi = true
			}
			out.WriteByte(' ')
			i++
			continue
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			out.WriteByte(c)
			i++
			continue
		}
		if afterSemi {
			stmts++
			afterSemi = false
		}
		sawText = true
		out.WriteByte(c)
		i++
	}
	return out.String(), stmts
}

// leadingKeyword returns the first bare word, uppercased, ignoring opening
// parentheses so `(SELECT ...) UNION ...` reads as SELECT.
func leadingKeyword(body string) string {
	s := strings.TrimLeft(body, " \t\r\n(")
	end := 0
	for end < len(s) && (isAlnum(s[end]) || s[end] == '_') {
		end++
	}
	return strings.ToUpper(s[:end])
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || isDigit(c)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
