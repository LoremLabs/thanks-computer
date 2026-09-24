package outlet

import (
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
)

func TestParseRef(t *testing.T) {
	ok := map[string][2]string{
		"outlet://crm/query":   {"crm", "query"},
		"outlet://crm/exec":    {"crm", "exec"},
		"outlet://a-b_2/query": {"a-b_2", "query"},
	}
	for in, want := range ok {
		n, op, err := ParseRef(in)
		if err != nil || n != want[0] || op != want[1] {
			t.Errorf("ParseRef(%q) = %q %q %v", in, n, op, err)
		}
	}
	bad := []string{
		"outlet://user:pw@crm/query",
		"outlet://crm:5432/query",
		"outlet://crm/query?x=1",
		"outlet://crm/query#f",
		"outlet://Crm/query",
		"outlet://crm/drop",
		"outlet://crm",
		"outlet:///query",
		"postgres://crm/query",
	}
	for _, in := range bad {
		if _, _, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q): expected an error", in)
		}
	}
}

func lit(s string) ast.Value { return ast.Literal{V: s} }

func TestCheckOp(t *testing.T) {
	decls := map[string]*Decl{
		"crm": {Driver: "postgres", Secret: "CRM_DSN", Access: AccessWrite},
		"ro":  {Driver: "postgres", Secret: "RO_DSN", Access: AccessRead},
	}
	type tc struct {
		name string
		exec string
		with map[string]ast.Value
		want string // substring of the error, "" = must pass
	}
	cases := []tc{
		{"select", "outlet://crm/query", map[string]ast.Value{"sql": lit("SELECT 1")}, ""},
		{"cte", "outlet://crm/query", map[string]ast.Value{"sql": lit("WITH x AS (SELECT 1) SELECT * FROM x")}, ""},
		{"paren", "outlet://crm/query", map[string]ast.Value{"sql": lit("(SELECT 1) UNION (SELECT 2)")}, ""},
		{"comment", "outlet://crm/query", map[string]ast.Value{"sql": lit("-- lookup; by email\nSELECT id FROM c WHERE e = $1")}, ""},
		{"semicolon in literal", "outlet://crm/query", map[string]ast.Value{"sql": lit("SELECT 1 WHERE n = 'a;b'")}, ""},
		{"trailing semicolon", "outlet://crm/query", map[string]ast.Value{"sql": lit("SELECT 1;")}, ""},
		{"dollar quoted", "outlet://crm/query", map[string]ast.Value{"sql": lit("SELECT $$a;b$$")}, ""},
		{"insert exec", "outlet://crm/exec", map[string]ast.Value{"sql": lit("INSERT INTO t (a) VALUES ($1) RETURNING id")}, ""},
		{"select exec", "outlet://crm/exec", map[string]ast.Value{"sql": lit("SELECT 1")}, ""},
		{"lowercase", "outlet://crm/query", map[string]ast.Value{"sql": lit("select 1")}, ""},

		{"unknown outlet", "outlet://nope/query", map[string]ast.Value{"sql": lit("SELECT 1")}, "not declared"},
		{"exec on read", "outlet://ro/exec", map[string]ast.Value{"sql": lit("SELECT 1")}, "access: read"},
		{"no sql", "outlet://crm/query", map[string]ast.Value{}, "needs WITH sql"},
		{"path sql", "outlet://crm/query", map[string]ast.Value{"sql": ast.PathRef{Path: "_txc.web.req.body"}}, "path reference"},
		{"concat sql", "outlet://crm/query", map[string]ast.Value{"sql": ast.FunctionCall{Name: "concat"}}, "&concat"},
		{"number sql", "outlet://crm/query", map[string]ast.Value{"sql": ast.Literal{V: int64(1)}}, "string literal"},
		{"empty sql", "outlet://crm/query", map[string]ast.Value{"sql": lit("  -- nothing\n")}, "empty"},
		{"two statements", "outlet://crm/query", map[string]ast.Value{"sql": lit("SELECT 1; DROP TABLE t")}, "more than one statement"},
		{"update under query", "outlet://crm/query", map[string]ast.Value{"sql": lit("UPDATE t SET a = 1")}, "use outlet://<name>/exec"},
		{"ddl", "outlet://crm/exec", map[string]ast.Value{"sql": lit("DROP TABLE t")}, "DDL"},
		{"truncate", "outlet://crm/exec", map[string]ast.Value{"sql": lit("TRUNCATE t")}, "DDL"},
		{"unknown verb", "outlet://crm/query", map[string]ast.Value{"sql": lit("EXPLAIN SELECT 1")}, "must start with"},
		{"userinfo", "outlet://u@crm/query", map[string]ast.Value{"sql": lit("SELECT 1")}, "credentials"},
	}
	for _, c := range cases {
		err := CheckOp(c.exec, c.with, decls)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", c.name, c.want, err)
		}
	}
	if err := CheckOp("outlet://crm/query", map[string]ast.Value{"sql": lit("SELECT 1")}, nil); err == nil || !strings.Contains(err.Error(), "add OUTLETS/crm.yaml") {
		t.Fatalf("no declarations at all should hint at the file: %v", err)
	}
}

func TestScanStatement(t *testing.T) {
	cases := map[string]int{
		"SELECT 1":                       1,
		"SELECT 1;":                      1,
		"SELECT 1; ":                     1,
		"SELECT 1; SELECT 2":             2,
		"SELECT ';' ; SELECT 2":          2,
		"SELECT ';'":                     1,
		"SELECT 1 /* ; */":               1,
		"SELECT 1 -- ;\n":                1,
		"SELECT $q$;$q$":                 1,
		"SELECT $1; SELECT $2":           2,
		"SELECT \"a;b\" FROM t":          1,
		"SELECT 'it''s; here'":           1,
		"/* a */ SELECT 1 /* b; c */ ; ": 1,
	}
	for in, want := range cases {
		if _, got := scanStatement(in); got != want {
			t.Errorf("scanStatement(%q) statements = %d, want %d", in, got, want)
		}
	}
	if kw := leadingKeyword("  ( ( with x as (select 1) select 1"); kw != "WITH" {
		t.Fatalf("leadingKeyword = %q", kw)
	}
}
