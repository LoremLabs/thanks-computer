package bundle

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestWalkABI(t *testing.T) {
	fsys := fstest.MapFS{
		"txco-web.json":                      {Data: []byte(`{"abi":1}`)},
		"ops/900000/spa-fallback.txcl":       {Data: []byte(`EMIT .a = 1`)},
		"ops/900000/mock-request.json":       {Data: []byte(`{ "x": 1 }`)},
		"ops/900900_terminal/not-found.txcl": {Data: []byte(`EMIT .b = 2`)},
		"ops/900000/shared.inc":              {Data: []byte(`EMIT .shared = true`)},
		"ops/900000/deep/er.txcl":            {Data: []byte(`&include("../shared.inc")`)}, // relative to the file
		"ops/.DS_Store":                      {Data: []byte("x")},
	}

	ops, err := WalkABI(fsys, "web", "www/txco-web")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Op{}
	for _, op := range ops {
		got[op.Name] = op
	}
	if len(ops) != 3 {
		t.Fatalf("want 3 ops, got %+v", ops)
	}
	fb := got["spa-fallback"]
	if fb.Stack != "web" || fb.Scope != 900000 || fb.Origin != OriginABI || fb.SourcePath != "www/txco-web/ops/900000/spa-fallback.txcl" {
		t.Errorf("spa-fallback = %+v", fb)
	}
	if fb.MockReq != `{"x":1}` {
		t.Errorf("mock = %q", fb.MockReq)
	}
	if nf := got["not-found"]; nf.Scope != 900900 {
		t.Errorf("a labeled scope dir: %+v", nf)
	}
	deep := got["deep_er"]
	if deep.Scope != 900000 || !strings.Contains(deep.Txcl, ".shared = true") || len(deep.Includes) != 1 ||
		deep.Includes[0] != "www/txco-web/ops/900000/shared.inc" {
		t.Errorf("nested + include: %+v", deep)
	}

	if ops, err := WalkABI(fstest.MapFS{"txco-web.json": {Data: []byte("{}")}}, "web", "x"); err != nil || ops != nil {
		t.Errorf("no ops/: %v %v", ops, err)
	}
}

func TestWalkABIRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		fsys fstest.MapFS
		want string
	}{
		"a top-level file":  {fstest.MapFS{"ops/x.txcl": {Data: []byte("EMIT .a = 1")}}, "numbered scope"},
		"a nested stack":    {fstest.MapFS{"ops/web/100/x.txcl": {Data: []byte("EMIT .a = 1")}}, "nested stack"},
		"a _ dir":           {fstest.MapFS{"ops/900000/_parked/x.txcl": {Data: []byte("EMIT .a = 1")}}, `"_" directory`},
		"a bad op name":     {fstest.MapFS{"ops/900000/we b.txcl": {Data: []byte("EMIT .a = 1")}}, "must match"},
		"a collision":       {fstest.MapFS{"ops/900000/a_b.txcl": {Data: []byte("EMIT .a = 1")}, "ops/900000/a/b.txcl": {Data: []byte("EMIT .a = 2")}}, "flatten to the same"},
		"TXCO_STACK_DIR":    {fstest.MapFS{"ops/900000/x.txcl": {Data: []byte(`WITH cwd = "$TXCO_STACK_DIR" EXEC "workspace://w/exec"`)}}, "TXCO_STACK_DIR"},
		"an include escape": {fstest.MapFS{"ops/900000/x.txcl": {Data: []byte(`&include("../txco-web.json")`)}, "txco-web.json": {Data: []byte("{}")}}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := WalkABI(c.fsys, "web", "abi")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
		})
	}
}
