package processor

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/stackdir"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

func TestWorkspaceCwd(t *testing.T) {
	cases := []struct {
		raw    string
		cwd    string
		inTree bool
		bad    bool
	}{
		{raw: "", cwd: ""},
		{raw: "foo", cwd: "foo"},
		{raw: "./foo/", cwd: "foo"},
		{raw: "$HOME", cwd: ""},
		{raw: "$HOME/project", cwd: "project"},
		{raw: "$HOME/foo/../bar", cwd: "bar"},
		{raw: "$HOME//x", cwd: "x"},
		{raw: "$HOME/../etc", bad: true},
		{raw: "..", bad: true},
		{raw: "a/../../b", bad: true},
		{raw: "$TXCO_STACK_DIR", cwd: "", inTree: true},
		{raw: "$TXCO_STACK_DIR/2100_SETUP", cwd: "2100_SETUP", inTree: true},
		{raw: "$TXCO_STACK_DIR/a/../b", cwd: "b", inTree: true},
		{raw: "$TXCO_STACK_DIR/../whatever", bad: true},
		{raw: "/tmp", cwd: "/tmp"},
		{raw: "/var/tmp/../tmp", cwd: "/var/tmp"}, // ".." undoes the first tmp
		{raw: "/tmp/x/../y", cwd: "/tmp/y"},
		{raw: "/opt/tool", cwd: "/opt/tool"},
		{raw: "/..", cwd: "/"},
		{raw: "$TMPDIR", bad: true},
		{raw: "$HOMEX", bad: true},
		{raw: "$TXCO_STACK_DIRX", bad: true},
	}
	for _, c := range cases {
		cwd, inTree, err := workspaceCwd(c.raw)
		if c.bad {
			if err == nil {
				t.Errorf("cwd %q: accepted as %q", c.raw, cwd)
			}
			continue
		}
		if err != nil || cwd != c.cwd || inTree != c.inTree {
			t.Errorf("cwd %q = (%q, %v, %v), want (%q, %v)", c.raw, cwd, inTree, err, c.cwd, c.inTree)
		}
	}
}

func TestWorkspaceRequestMarksTheTree(t *testing.T) {
	req, err := workspaceRequest(workspaceOp("workspace://browser/exec", `{"args":["python3","race.py"],"cwd":"$TXCO_STACK_DIR/2100_SETUP"}`))
	if err != nil || req.Tree == nil || req.Cwd != "2100_SETUP" {
		t.Fatalf("req = %+v, %v", req, err)
	}
	req, err = workspaceRequest(workspaceOp("workspace://browser/exec", `{"command":"x","cwd":"/tmp"}`))
	if err != nil || req.Tree != nil || req.Cwd != "/tmp" {
		t.Fatalf("absolute cwd: %+v, %v", req, err)
	}
	if _, err := workspaceRequest(workspaceOp("workspace://browser/exec", `{"command":"x","cwd":"$HOME/../x"}`)); err == nil {
		t.Fatal("an escaping cwd was accepted")
	}
}

type mapBundles map[string][]byte

func (m mapBundles) Get(_ context.Context, hash string) ([]byte, error) {
	if b, ok := m[hash]; ok {
		return b, nil
	}
	return nil, errors.New("not found")
}

// stackTables adds the runtime tables stackTree reads to the test unit's DB,
// which seeds only ops and tenants.
func stackTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS stacks (stack_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, name TEXT NOT NULL, active_version INTEGER, created_at TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS stack_versions (version_id INTEGER PRIMARY KEY, stack_id TEXT NOT NULL, version_number INTEGER NOT NULL, status TEXT NOT NULL, created_by TEXT NOT NULL, created_at TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS stack_files (version_id INTEGER NOT NULL, path TEXT NOT NULL, content TEXT NOT NULL, content_hash TEXT NOT NULL DEFAULT '', PRIMARY KEY (version_id, path));`); err != nil {
		t.Fatal(err)
	}
}

// seedTree makes `name` in tenant acme an active stack whose version carries
// the tree of files, and returns its digest and bundle.
func seedTree(t *testing.T, db *sql.DB, version int, name string, files ...stackdir.File) (string, []byte) {
	t.Helper()
	data, digest, err := stackdir.Pack(files)
	if err != nil {
		t.Fatal(err)
	}
	stmts := []struct {
		q    string
		args []any
	}{
		{`INSERT OR IGNORE INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES (?, 'tnt_acme', ?, NULL, '')`, []any{"stk_" + name, name}},
		{`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at) VALUES (?, ?, ?, 'superseded', 't', '')`, []any{version, "stk_" + name, version}},
		{`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (?, ?, '', ?)`, []any{version, stackdir.Path(digest), digest}},
		{`UPDATE stacks SET active_version = ? WHERE stack_id = ?`, []any{version, "stk_" + name}},
	}
	for _, s := range stmts {
		if _, err := db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("seed %s: %v", s.q, err)
		}
	}
	return digest, data
}

// The run's tree comes from the snapshot it pinned: a continuation resumes
// with the tree it suspended with, whatever was applied in between.
func TestStackTreeFollowsThePinnedSnapshot(t *testing.T) {
	pu, _ := newTestUnit(t)
	db := pu.Dbc.Db
	if _, err := db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme','acme','acme','')`); err != nil {
		t.Fatal(err)
	}
	stackTables(t, db)
	d1, b1 := seedTree(t, db, 1, "game", stackdir.File{Path: "race.py", Content: []byte("v1")})
	bundles := mapBundles{d1: b1}
	pu.TreeBundles = bundles

	live := context.WithValue(WithTenant(context.Background(), "acme"), ctxKeyOpstackSnap, db)
	tree, code, msg := pu.stackTree(live, "acme", "game")
	if code != "" || tree.Digest != d1 {
		t.Fatalf("live tree = %+v, %s %s", tree, code, msg)
	}
	if b, err := tree.Open(live); err != nil || string(b) != string(b1) {
		t.Fatalf("open = %v", err)
	}

	// Suspend: the snapshot carries the digest.
	data, _, n, err := pu.snapshotOpstack(live, "acme")
	if err != nil {
		t.Fatal(err)
	}
	_ = n
	if !strings.Contains(string(data), d1) {
		t.Fatalf("snapshot has no tree digest: %s", data)
	}
	frozen, err := buildSnapshotDB(data)
	if err != nil {
		t.Fatal(err)
	}
	defer frozen.Close()

	// An apply between suspend and resume.
	d2, b2 := seedTree(t, db, 2, "game", stackdir.File{Path: "race.py", Content: []byte("v2")})
	bundles[d2] = b2

	resumed := context.WithValue(WithTenant(context.Background(), "acme"), ctxKeyOpstackSnap, frozen)
	if tree, code, msg := pu.stackTree(resumed, "acme", "game"); code != "" || tree.Digest != d1 {
		t.Fatalf("resumed tree = %+v, %s %s; want the suspended one", tree, code, msg)
	}
	if tree, code, _ := pu.stackTree(live, "acme", "game"); code != "" || tree.Digest != d2 {
		t.Fatalf("a new run should see the new tree, got %+v %s", tree, code)
	}

	// A stack without a tree, and a chassis without a file store, say so.
	if _, code, _ := pu.stackTree(live, "acme", "other"); code != workspace.CodeStackDirUnavailable {
		t.Fatalf("no tree: code %q", code)
	}
	pu.TreeBundles = nil
	if _, code, _ := pu.stackTree(live, "acme", "game"); code != workspace.CodeStackDirUnavailable {
		t.Fatalf("no store: code %q", code)
	}
}

// A tenant with no trees snapshots exactly as before trees existed.
func TestSnapshotWithoutTreesIsUnchanged(t *testing.T) {
	pu, _ := newTestUnit(t)
	db := pu.Dbc.Db
	if _, err := db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme','acme','acme','')`); err != nil {
		t.Fatal(err)
	}
	stackTables(t, db)
	ctx := context.WithValue(WithTenant(context.Background(), "acme"), ctxKeyOpstackSnap, db)
	data, _, _, err := pu.snapshotOpstack(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "stack_trees") {
		t.Fatalf("an empty tree list was written: %s", data)
	}
}
