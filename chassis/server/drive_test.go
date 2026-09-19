package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
	"github.com/loremlabs/thanks-computer/chassis/blob"
	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/drive/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
	casfs "github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// newDriveDeps builds the op deps over a temp SQLite index + file object
// store, with a mirror DB that owns exactly the given (hostname → tenant)
// pairs.
func newDriveDeps(t *testing.T, owned map[string]string) driveDeps {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "drive.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	objects, err := filestore.New(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	store := chdrive.NewStore(db, registry.SQLite, objects)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	mirror, err := sql.Open("sqlite3", filepath.Join(dir, "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	for _, q := range []string{
		`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`,
		`CREATE TABLE tenant_hostnames (hostname TEXT, tenant_id TEXT, verified_at TEXT, revoked_at TEXT)`,
		`CREATE TABLE dns_zones (origin TEXT, tenant_id TEXT, verified_at TEXT, revoked_at TEXT)`,
	} {
		if _, err := mirror.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for host, slug := range owned {
		if _, err := mirror.Exec(`INSERT OR IGNORE INTO tenants VALUES (?, ?, NULL)`, "t_"+slug, slug); err != nil {
			t.Fatal(err)
		}
		if _, err := mirror.Exec(`INSERT INTO tenant_hostnames VALUES (?, ?, '2026-09-16T00:00:00Z', NULL)`, host, "t_"+slug); err != nil {
			t.Fatal(err)
		}
	}
	fcas, err := casfs.New(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	return driveDeps{store: store, ids: authntest.NewSQLiteStore(t), snap: func() *sql.DB { return mirror }, dialect: registry.SQLite,
		maxBytes: 1 << 20, prefix: "/drive", now: func() time.Time { return fixed },
		ix: blob.NewKVIndex(newKVHandle(t)), fcas: fcas}
}

func callDrive(t *testing.T, fn func(context.Context, driveDeps, []byte) (event.Payload, error), d driveDeps, tenant, metaJSON string) string {
	t.Helper()
	return callDriveFrom(t, fn, d, tenant, "web", metaJSON)
}

// callDriveFrom runs the op as a rule of `stack`.
func callDriveFrom(t *testing.T, fn func(context.Context, driveDeps, []byte) (event.Payload, error), d driveDeps, tenant, stack, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	if stack != "" {
		ctx = processor.WithStack(ctx, stack)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	pl, err := fn(ctx, d, []byte(`{"_txc":{"op":"demo/100/drive"},"body":{"content_b64":"aGVsbG8="}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func TestDriveCollectionAndAccountOps(t *testing.T) {
	d := newDriveDeps(t, map[string]string{"pony.example.com": "acme"})

	out := callDrive(t, driveCollection, d, "acme", `{"name":"paris"}`)
	if gjson.Get(out, "_drive.error").Exists() || !gjson.Get(out, "_drive.created").Bool() || gjson.Get(out, "_drive.name").String() != "paris" ||
		!strings.HasPrefix(gjson.Get(out, "_drive.id").String(), "dc_") {
		t.Fatalf("collection create = %s", out)
	}
	collID := gjson.Get(out, "_drive.id").String()
	out = callDrive(t, driveCollection, d, "acme", `{"name":"paris"}`)
	if gjson.Get(out, "_drive.created").Bool() || gjson.Get(out, "_drive.id").String() != collID {
		t.Fatalf("collection ensure = %s", out)
	}
	if got := gjson.Get(callDrive(t, driveCollection, d, "acme", `{"name":"bad name"}`), "_drive.error.code").String(); got != "txco_drive_invalid_arg" {
		t.Errorf("bad name → %s", got)
	}

	out = callDrive(t, driveAccount, d, "acme", `{"username":"Paris@Pony.Example.com","collection":"paris","principal":"pony:paris"}`)
	if gjson.Get(out, "_drive.error").Exists() {
		t.Fatalf("account create: %s", out)
	}
	if gjson.Get(out, "_drive.username").String() != "paris@pony.example.com" || !gjson.Get(out, "_drive.created").Bool() ||
		gjson.Get(out, "_drive.principal").String() != "pony:paris" || gjson.Get(out, "_drive.password").Exists() ||
		gjson.Get(out, "_drive.collection_id").String() != collID ||
		gjson.Get(out, "_drive.collection").String() != "paris" || gjson.Get(out, "_drive.mount").String() != "/drive/paris/" {
		t.Errorf("create = %s", out)
	}
	a, ok, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
	if !ok || a.Tenant != "acme" || a.CollectionID != collID {
		t.Fatalf("account = %+v ok=%v", a, ok)
	}
	wantBound(t, d.ids, "t_acme", "paris@pony.example.com", "pony:paris")
	// An update needs neither the principal nor the collection.
	out = callDrive(t, driveAccount, d, "acme", `{"username":"paris@pony.example.com","status":"disabled","into":"_da"}`)
	if gjson.Get(out, "_da.error").Exists() || gjson.Get(out, "_da.created").Bool() ||
		gjson.Get(out, "_da.collection").String() != "paris" || gjson.Get(out, "_da.principal").String() != "pony:paris" {
		t.Errorf("update = %s", out)
	}
	accountOpRefusals(t, "drive", "paris@pony.example.com", `,"collection":"paris"`, func(stack, meta string) string {
		return callDriveFrom(t, driveAccount, d, "acme", stack, meta)
	})
	for meta, code := range map[string]string{
		`{"username":"x@else.example.com","collection":"paris"}`:                     "txco_drive_domain_not_owned",
		`{"username":"nope","collection":"paris"}`:                                   "txco_drive_invalid_arg",
		`{"username":"x@pony.example.com"}`:                                          "txco_drive_invalid_arg",
		`{"username":"x@pony.example.com","collection":"nope","principal":"pony:x"}`: "txco_drive_not_found",
	} {
		if got := gjson.Get(callDrive(t, driveAccount, d, "acme", meta), "_drive.error.code").String(); got != code {
			t.Errorf("%s → %s, want %s", meta, got, code)
		}
	}
	if got := gjson.Get(callDrive(t, driveAccount, d, "", `{"username":"paris@pony.example.com"}`), "_drive.error.code").String(); got != "txco_drive_no_tenant" {
		t.Errorf("no tenant → %s", got)
	}
	if got := gjson.Get(callDrive(t, driveAccount, driveDeps{}, "acme", `{"username":"paris@pony.example.com"}`), "_drive.error.code").String(); got != "txco_drive_disabled" {
		t.Errorf("no store → %s", got)
	}
	// Remove: refused non-empty, then forced.
	callDrive(t, drivePut, d, "acme", `{"collection":"paris","path":"a.txt","value":"aGk=","into":"_p"}`)
	if got := gjson.Get(callDrive(t, driveCollection, d, "acme", `{"name":"paris","remove":true}`), "_drive.error.code").String(); got != "txco_drive_not_empty" {
		t.Errorf("remove non-empty → %s", got)
	}
	out = callDrive(t, driveCollection, d, "acme", `{"name":"paris","remove":true,"force":true}`)
	if !gjson.Get(out, "_drive.removed").Bool() {
		t.Errorf("remove forced = %s", out)
	}
	if got := gjson.Get(callDrive(t, driveStat, d, "acme", `{"collection":"paris","path":"a.txt"}`), "_drive.error.code").String(); got != "txco_drive_not_found" {
		t.Errorf("stat after remove → %s", got)
	}
}

func TestDriveFileOps(t *testing.T) {
	d := newDriveDeps(t, map[string]string{"pony.example.com": "acme"})
	callDrive(t, driveCollection, d, "acme", `{"name":"docs"}`)
	callDrive(t, driveCollection, d, "other", `{"name":"docs"}`) // another tenant's, same name

	// Bytes from the envelope, base64 default.
	out := callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"/notes/hello.txt","from":"body.content_b64","into":"_p"}`)
	if got := gjson.Get(out, "_p.error.code").String(); got != "txco_drive_no_parent" {
		t.Fatalf("put without parent = %s", out)
	}
	out = callDrive(t, driveMkdir, d, "acme", `{"collection":"docs","path":"notes"}`)
	if gjson.Get(out, "_drive.error").Exists() || !gjson.Get(out, "_drive.created").Bool() {
		t.Fatalf("mkdir = %s", out)
	}
	out = callDrive(t, driveMkdir, d, "acme", `{"collection":"docs","path":"notes"}`)
	if gjson.Get(out, "_drive.error").Exists() || gjson.Get(out, "_drive.created").Bool() {
		t.Fatalf("mkdir twice = %s", out)
	}
	out = callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"notes/hello.txt","from":"body.content_b64","into":"_p"}`)
	if gjson.Get(out, "_p.error").Exists() || !gjson.Get(out, "_p.created").Bool() || gjson.Get(out, "_p.size").Int() != 5 ||
		gjson.Get(out, "_p.path").String() != "notes/hello.txt" || gjson.Get(out, "_p.etag").String() != chdrive.ETagOf([]byte("hello")) {
		t.Fatalf("put = %s", out)
	}
	id := gjson.Get(out, "_p.resource_id").String()
	if got := gjson.Get(callDrive(t, driveMkdir, d, "acme", `{"collection":"docs","path":"notes/hello.txt"}`), "_drive.error.code").String(); got != "txco_drive_exists" {
		t.Errorf("mkdir over file → %s", got)
	}
	// utf8 literal, content type, conditional.
	out = callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"notes/hello.txt","value":"hello again","encoding":"utf8","content_type":"text/markdown","if_match":"nope"}`)
	if got := gjson.Get(out, "_drive.error.code").String(); got != "txco_drive_precondition" {
		t.Errorf("if_match → %s", got)
	}
	out = callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"notes/hello.txt","value":"hello again","encoding":"utf8","content_type":"text/markdown"}`)
	if gjson.Get(out, "_drive.created").Bool() || gjson.Get(out, "_drive.resource_id").String() != id || gjson.Get(out, "_drive.modseq").Int() != 3 {
		t.Errorf("replace = %s", out)
	}

	// get by path and by id, encodings, cap.
	out = callDrive(t, driveGet, d, "acme", `{"collection":"docs","path":"notes/hello.txt","encoding":"utf8"}`)
	if gjson.Get(out, "_drive.content").String() != "hello again" || gjson.Get(out, "_drive.content_type").String() != "text/markdown" ||
		gjson.Get(out, "_drive.name").String() != "hello.txt" || gjson.Get(out, "_drive.resource_id").String() != id {
		t.Errorf("get = %s", out)
	}
	out = callDrive(t, driveGet, d, "acme", `{"collection":"docs","resource_id":"`+id+`"}`)
	if b, _ := base64.StdEncoding.DecodeString(gjson.Get(out, "_drive.content").String()); string(b) != "hello again" || gjson.Get(out, "_drive.encoding").String() != "base64" {
		t.Errorf("get by id = %s", out)
	}
	if got := gjson.Get(callDrive(t, driveGet, d, "acme", `{"collection":"docs","path":"notes/hello.txt","max_bytes":3}`), "_drive.error.code").String(); got != "txco_drive_too_large" {
		t.Errorf("max_bytes → %s", got)
	}
	if got := gjson.Get(callDrive(t, driveGet, d, "acme", `{"collection":"docs","path":"notes"}`), "_drive.error.code").String(); got != "txco_drive_is_directory" {
		t.Errorf("get dir → %s", got)
	}
	if got := gjson.Get(callDrive(t, driveGet, d, "acme", `{"collection":"docs","path":"x","resource_id":"y"}`), "_drive.error.code").String(); got != "txco_drive_invalid_arg" {
		t.Errorf("both addresses → %s", got)
	}
	// The other tenant's same-named collection is empty.
	if got := gjson.Get(callDrive(t, driveGet, d, "other", `{"collection":"docs","path":"notes/hello.txt"}`), "_drive.error.code").String(); got != "txco_drive_not_found" {
		t.Errorf("cross-tenant get → %s", got)
	}

	// stat: file, root, miss.
	out = callDrive(t, driveStat, d, "acme", `{"collection":"docs","path":"notes/hello.txt"}`)
	if !gjson.Get(out, "_drive.exists").Bool() || gjson.Get(out, "_drive.resource.kind").String() != "file" || gjson.Get(out, "_drive.resource.size").Int() != 11 {
		t.Errorf("stat = %s", out)
	}
	out = callDrive(t, driveStat, d, "acme", `{"collection":"docs","path":""}`)
	if !gjson.Get(out, "_drive.exists").Bool() || gjson.Get(out, "_drive.resource.kind").String() != "dir" || gjson.Get(out, "_drive.resource.modseq").Int() != 3 {
		t.Errorf("stat root = %s", out)
	}
	if out := callDrive(t, driveStat, d, "acme", `{"collection":"docs","path":"nope"}`); gjson.Get(out, "_drive.exists").Bool() || gjson.Get(out, "_drive.error").Exists() {
		t.Errorf("stat miss = %s", out)
	}

	// list: children, recursive, since + deleted, paging.
	callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"notes/b.txt","value":"b","encoding":"utf8"}`)
	callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"top.txt","value":"t","encoding":"utf8"}`)
	paths := func(out string) string {
		var ps []string
		for _, it := range gjson.Get(out, "_drive.items").Array() {
			ps = append(ps, it.Get("path").String())
		}
		return strings.Join(ps, ",")
	}
	out = callDrive(t, driveList, d, "acme", `{"collection":"docs"}`)
	if paths(out) != "notes,top.txt" || gjson.Get(out, "_drive.count").Int() != 2 || gjson.Get(out, "_drive.sync_token").Int() != 5 || gjson.Get(out, "_drive.next").String() != "" {
		t.Errorf("list root = %s", out)
	}
	out = callDrive(t, driveList, d, "acme", `{"collection":"docs","path":"notes"}`)
	if paths(out) != "notes/b.txt,notes/hello.txt" {
		t.Errorf("list notes = %s", out)
	}
	out = callDrive(t, driveList, d, "acme", `{"collection":"docs","recursive":true,"limit":2}`)
	if paths(out) != "notes,notes/b.txt" || gjson.Get(out, "_drive.next").String() != "notes/b.txt" {
		t.Errorf("list page 1 = %s", out)
	}
	out = callDrive(t, driveList, d, "acme", `{"collection":"docs","recursive":true,"limit":2,"after":"notes/b.txt"}`)
	if paths(out) != "notes/hello.txt,top.txt" || gjson.Get(out, "_drive.next").String() != "" {
		t.Errorf("list page 2 = %s", out)
	}
	if got := gjson.Get(callDrive(t, driveList, d, "acme", `{"collection":"docs","path":"top.txt"}`), "_drive.error.code").String(); got != "txco_drive_not_directory" {
		t.Errorf("list a file → %s", got)
	}
	if got := gjson.Get(callDrive(t, driveList, d, "acme", `{"collection":"docs","path":"nope"}`), "_drive.error.code").String(); got != "txco_drive_not_found" {
		t.Errorf("list missing dir → %s", got)
	}

	// move / copy / delete.
	out = callDrive(t, driveMove, d, "acme", `{"collection":"docs","path":"notes/hello.txt","to":"top.txt"}`)
	if got := gjson.Get(out, "_drive.error.code").String(); got != "txco_drive_exists" {
		t.Errorf("move without overwrite → %s", got)
	}
	out = callDrive(t, driveMove, d, "acme", `{"collection":"docs","path":"notes/hello.txt","to":"top.txt","overwrite":true}`)
	if gjson.Get(out, "_drive.error").Exists() || gjson.Get(out, "_drive.resource_id").String() != id || gjson.Get(out, "_drive.from").String() != "notes/hello.txt" {
		t.Errorf("move = %s", out)
	}
	out = callDrive(t, driveCopy, d, "acme", `{"collection":"docs","path":"notes","to":"notes2"}`)
	if gjson.Get(out, "_drive.error").Exists() || gjson.Get(out, "_drive.kind").String() != "dir" {
		t.Errorf("copy = %s", out)
	}
	if got := gjson.Get(callDrive(t, driveCopy, d, "acme", `{"collection":"docs","path":"notes","to":"notes/in"}`), "_drive.error.code").String(); got != "txco_drive_cycle" {
		t.Errorf("copy into self → %s", got)
	}
	out = callDrive(t, driveList, d, "acme", `{"collection":"docs","recursive":true}`)
	if paths(out) != "notes,notes/b.txt,notes2,notes2/b.txt,top.txt" {
		t.Errorf("after move/copy = %s", paths(out))
	}
	since := gjson.Get(out, "_drive.sync_token").Int()
	out = callDrive(t, driveDelete, d, "acme", `{"collection":"docs","path":"notes"}`)
	if !gjson.Get(out, "_drive.deleted").Bool() || gjson.Get(out, "_drive.kind").String() != "dir" {
		t.Errorf("delete = %s", out)
	}
	if out := callDrive(t, driveDelete, d, "acme", `{"collection":"docs","path":"notes"}`); gjson.Get(out, "_drive.deleted").Bool() || gjson.Get(out, "_drive.error").Exists() {
		t.Errorf("delete twice = %s", out)
	}
	out = callDrive(t, driveDelete, d, "acme", `{"collection":"docs","resource_id":"`+id+`","if_match":"nope"}`)
	if got := gjson.Get(out, "_drive.error.code").String(); got != "txco_drive_precondition" {
		t.Errorf("delete if_match → %s", got)
	}
	out = callDrive(t, driveList, d, "acme", `{"collection":"docs","since":`+itoa(since)+`,"include_deleted":true}`)
	if paths(out) != "notes,notes/b.txt" {
		t.Errorf("since+deleted = %s", out)
	}
	for _, it := range gjson.Get(out, "_drive.items").Array() {
		if !it.Get("deleted").Bool() || it.Get("deleted_at").String() == "" {
			t.Errorf("tombstone item = %s", it.Raw)
		}
	}
	// Op-level byte cap.
	d.maxBytes = 4
	if got := gjson.Get(callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"big","value":"12345","encoding":"utf8"}`), "_drive.error.code").String(); got != "txco_drive_too_large" {
		t.Errorf("op cap put → %s", got)
	}
	if got := gjson.Get(callDrive(t, driveGet, d, "acme", `{"collection":"docs","path":"top.txt"}`), "_drive.error.code").String(); got != "txco_drive_too_large" {
		t.Errorf("op cap get → %s", got)
	}
}

// TestDrivePutFromShaAndParents: bytes the tenant already holds in the
// blob store land in the drive by sha (ownership-checked, not through the
// envelope), and `parents` makes the directories on the way.
func TestDrivePutFromShaAndParents(t *testing.T) {
	d := newDriveDeps(t, map[string]string{"pony.example.com": "acme"})
	callDrive(t, driveCollection, d, "acme", `{"name":"docs"}`)
	callDrive(t, driveCollection, d, "rival", `{"name":"docs"}`)
	bd := blobDeps{fcas: d.fcas, ix: d.ix, maxBytes: 1 << 20, now: d.now}
	bout := callBlob(t, blobPut, bd, "acme", `{"name":"docs/seed","value":"from the cas","encoding":"utf8","grants":[]}`, "")
	sha := gjson.Get(bout, "_blob.sha256").String()
	if gjson.Get(bout, "blob.error").Exists() || sha == "" {
		t.Fatalf("seed blob = %s", bout)
	}

	// parents: three levels made on the way; the put lands; a re-run is a noop.
	out := callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"Knowledge/reports/2026/q3.txt","from_sha":"`+sha+`","content_type":"text/plain","parents":true,"into":"_p"}`)
	if gjson.Get(out, "_p.error").Exists() || !gjson.Get(out, "_p.created").Bool() || gjson.Get(out, "_p.size").Int() != int64(len("from the cas")) ||
		gjson.Get(out, "_p.etag").String() != chdrive.ETagOf([]byte("from the cas")) {
		t.Fatalf("put from_sha = %s", out)
	}
	out = callDrive(t, driveGet, d, "acme", `{"collection":"docs","path":"Knowledge/reports/2026/q3.txt","encoding":"utf8"}`)
	if gjson.Get(out, "_drive.content").String() != "from the cas" || gjson.Get(out, "_drive.content_type").String() != "text/plain" {
		t.Errorf("get = %s", out)
	}
	out = callDrive(t, driveList, d, "acme", `{"collection":"docs","recursive":true}`)
	var ps []string
	for _, it := range gjson.Get(out, "_drive.items").Array() {
		ps = append(ps, it.Get("kind").String()+":"+it.Get("path").String())
	}
	if got := strings.Join(ps, ","); got != "dir:Knowledge,dir:Knowledge/reports,dir:Knowledge/reports/2026,file:Knowledge/reports/2026/q3.txt" {
		t.Errorf("tree = %s", got)
	}
	out = callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"Knowledge/reports/2026/q3.txt","from_sha":"`+sha+`","parents":true}`)
	if !gjson.Get(out, "_drive.noop").Bool() {
		t.Errorf("same bytes again = %s", out)
	}
	// The op cap does not apply to a streamed object.
	d.maxBytes = 4
	out = callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"Knowledge/again.txt","from_sha":"`+sha+`"}`)
	if gjson.Get(out, "_drive.error").Exists() {
		t.Errorf("streamed put under a tiny op cap = %s", out)
	}
	d.maxBytes = 1 << 20

	// Refusals: another tenant, an unknown sha, a bad sha, both sources, no store.
	for meta, code := range map[string]string{
		`{"collection":"docs","path":"x","from_sha":"` + sha + `"}`:                     "txco_drive_not_found",
		`{"collection":"docs","path":"x","from_sha":"` + strings.Repeat("0", 64) + `"}`: "txco_drive_not_found",
		`{"collection":"docs","path":"x","from_sha":"nope"}`:                            "txco_drive_invalid_arg",
		`{"collection":"docs","path":"x","from_sha":"` + sha + `","value":"aGk="}`:      "txco_drive_invalid_arg",
	} {
		tenant := "acme"
		if code == "txco_drive_not_found" && strings.Contains(meta, sha) {
			tenant = "rival"
		}
		if got := gjson.Get(callDrive(t, drivePut, d, tenant, meta), "_drive.error.code").String(); got != code {
			t.Errorf("%s (%s) → %s, want %s", meta, tenant, got, code)
		}
	}
	off := d
	off.ix, off.fcas = nil, nil
	if got := gjson.Get(callDrive(t, drivePut, off, "acme", `{"collection":"docs","path":"x","from_sha":"`+sha+`"}`), "_drive.error.code").String(); got != "txco_drive_disabled" {
		t.Errorf("no blob store → %s", got)
	}

	// parents on mkdir and move; a file in the way is still refused.
	out = callDrive(t, driveMkdir, d, "acme", `{"collection":"docs","path":"Input/Knowledge","parents":true}`)
	if gjson.Get(out, "_drive.error").Exists() || !gjson.Get(out, "_drive.created").Bool() {
		t.Errorf("mkdir parents = %s", out)
	}
	out = callDrive(t, driveMove, d, "acme", `{"collection":"docs","path":"Knowledge/reports/2026/q3.txt","to":"Archive/2026/q3.txt","parents":true}`)
	if gjson.Get(out, "_drive.error").Exists() || gjson.Get(out, "_drive.path").String() != "Archive/2026/q3.txt" {
		t.Errorf("move parents = %s", out)
	}
	if got := gjson.Get(callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"Archive/2026/q3.txt/inside.txt","value":"aGk=","parents":true}`), "_drive.error.code").String(); got != "txco_drive_not_directory" {
		t.Errorf("parents through a file → %s", got)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The `policy` argument is how a stack reserves a subtree for itself: the
// head refuses those verbs to a WebDAV client, while these ops (the stack's
// own hands) stay unrestricted.
func TestDriveCollectionPolicyOp(t *testing.T) {
	d := newDriveDeps(t, map[string]string{"pony.example.com": "acme"})

	// An ensure without `policy` leaves a collection open.
	out := callDrive(t, driveCollection, d, "acme", `{"name":"paris"}`)
	if gjson.Get(out, "_drive.policy").Exists() {
		t.Fatalf("a new collection carries a policy: %s", out)
	}
	// Setting one reads back, and the store agrees.
	out = callDrive(t, driveCollection, d, "acme", `{"name":"paris","policy":{"Knowledge":{"write":"deny"}}}`)
	if gjson.Get(out, "_drive.error").Exists() || gjson.Get(out, "_drive.policy.Knowledge.write").String() != "deny" {
		t.Fatalf("set policy = %s", out)
	}
	c, found, err := d.store.GetCollection(context.Background(), "acme", "paris")
	if err != nil || !found {
		t.Fatalf("GetCollection: %v %v", err, found)
	}
	if c.Policy.Allows("Knowledge/a.pdf", chdrive.VerbWrite) {
		t.Error("the stored policy does not refuse the write")
	}
	// An ensure that does not mention policy must not disturb it.
	out = callDrive(t, driveCollection, d, "acme", `{"name":"paris"}`)
	if gjson.Get(out, "_drive.policy.Knowledge.write").String() != "deny" {
		t.Fatalf("a plain ensure cleared the policy: %s", out)
	}
	// The ops themselves are never subject to it.
	out = callDrive(t, drivePut, d, "acme", `{"collection":"paris","path":"Knowledge/a.pdf","value":"aGk=","parents":true,"into":"_p"}`)
	if gjson.Get(out, "_p.error").Exists() {
		t.Fatalf("the policy leaked into the ops: %s", out)
	}
	// A typo is refused rather than silently allowing what it meant to deny.
	for _, meta := range []string{
		`{"name":"paris","policy":{"Knowledge":{"put":"deny"}}}`,
		`{"name":"paris","policy":{"Knowledge":{"write":"refuse"}}}`,
		`{"name":"paris","policy":"deny"}`,
	} {
		if got := gjson.Get(callDrive(t, driveCollection, d, "acme", meta), "_drive.error.code").String(); got != "txco_drive_invalid_arg" {
			t.Errorf("%s → %s, want txco_drive_invalid_arg", meta, got)
		}
	}
	// An empty object clears it.
	out = callDrive(t, driveCollection, d, "acme", `{"name":"paris","policy":{}}`)
	if gjson.Get(out, "_drive.policy").Exists() {
		t.Fatalf("an empty policy did not clear: %s", out)
	}
}
