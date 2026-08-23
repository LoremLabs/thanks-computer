package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The blob handler tests exercise the spec's acceptance list
// (thanks-computer-service/docs/todo-blob-ops.md) against a boltdb index +
// a file CAS in t.TempDir, calling the handlers the way ExecCore does:
// trusted tenant on ctx + WITH meta.

func newBlobDeps(t *testing.T, maxBytes int64) blobDeps {
	t.Helper()
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	return blobDeps{fcas: fs, ix: blob.NewKVIndex(newKVHandle(t)), maxBytes: maxBytes, now: func() time.Time { return fixed }}
}

type blobHandler func(context.Context, blobDeps, []byte) (event.Payload, error)

// callBlob runs a blob handler for tenant with the given WITH meta; the
// envelope carries `_txc.op` (the fuel stage) plus inExtra merged in.
func callBlob(t *testing.T, fn blobHandler, d blobDeps, tenant, metaJSON, inExtra string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	in := `{"_txc":{"op":"demo/100/blob"}}`
	if inExtra != "" {
		gjson.Parse(inExtra).ForEach(func(key, val gjson.Result) bool {
			var err error
			in, err = sjson.SetRaw(in, key.String(), val.Raw)
			if err != nil {
				t.Fatal(err)
			}
			return true
		})
	}
	pl, err := fn(ctx, d, []byte(in))
	if err != nil {
		t.Fatalf("blob handler returned a Go error (must be envelope-surfaced): %v", err)
	}
	return pl.Raw
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func wantCode(t *testing.T, raw, code string) {
	t.Helper()
	if got := gjson.Get(raw, "blob.error.code").String(); got != code {
		t.Fatalf("blob.error.code = %q, want %q (raw %s)", got, code, raw)
	}
}

func wantOK(t *testing.T, raw string) {
	t.Helper()
	if e := gjson.Get(raw, "blob.error"); e.Exists() {
		t.Fatalf("unexpected blob.error: %s", e.Raw)
	}
}

func TestBlobPutGetRoundTripBinary(t *testing.T) {
	d := newBlobDeps(t, 0)
	bin := []byte{0x00, 0xff, 0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00}
	raw := callBlob(t, blobPut, d, "t1",
		`{"name":"docs/logo.bin","value":"`+b64(bin)+`"}`, "")
	wantOK(t, raw)
	if gjson.Get(raw, "_blob.sha256").String() != shaOf(bin) || gjson.Get(raw, "_blob.size").Int() != int64(len(bin)) ||
		gjson.Get(raw, "_blob.existed").Bool() || gjson.Get(raw, "_blob.replaced").Bool() ||
		gjson.Get(raw, "_blob.name").String() != "docs/logo.bin" {
		t.Fatalf("put result: %s", raw)
	}
	// Default get encoding is base64 — binary-safe.
	got := callBlob(t, blobGet, d, "t1", `{"name":"docs/logo.bin","into":"_out"}`, "")
	wantOK(t, got)
	dec, err := base64.StdEncoding.DecodeString(gjson.Get(got, "_out.content").String())
	if err != nil || string(dec) != string(bin) {
		t.Fatalf("round-trip bytes differ: %s (err %v)", got, err)
	}
	if gjson.Get(got, "_out.encoding").String() != "base64" || gjson.Get(got, "_out.content_type").String() != "application/octet-stream" ||
		gjson.Get(got, "_out.name").String() != "docs/logo.bin" || gjson.Get(got, "_out.size").Int() != int64(len(bin)) {
		t.Fatalf("get metadata: %s", got)
	}
	// `from` an envelope path works the same as `value`.
	raw = callBlob(t, blobPut, d, "t1", `{"name":"docs/copy.bin","from":"@web.req.body"}`,
		`{"_txc.web.req.body":"`+b64(bin)+`"}`)
	wantOK(t, raw)
	if gjson.Get(raw, "_blob.sha256").String() != shaOf(bin) || !gjson.Get(raw, "_blob.existed").Bool() {
		t.Fatalf("from-path put: %s", raw)
	}
}

func TestBlobUtf8PutAutoGet(t *testing.T) {
	d := newBlobDeps(t, 0)
	raw := callBlob(t, blobPut, d, "t1", `{"name":"faqs/a.txt","value":"hello, world","encoding":"utf8"}`, "")
	wantOK(t, raw)
	got := callBlob(t, blobGet, d, "t1", `{"name":"faqs/a.txt","encoding":"auto"}`, "")
	wantOK(t, got)
	if gjson.Get(got, "_blob.content").String() != "hello, world" || gjson.Get(got, "_blob.encoding").String() != "utf8" ||
		gjson.Get(got, "_blob.content_type").String() != "text/plain; charset=utf-8" {
		t.Fatalf("auto get: %s", got)
	}
	got = callBlob(t, blobGet, d, "t1", `{"name":"faqs/a.txt","encoding":"utf8"}`, "")
	if gjson.Get(got, "_blob.content").String() != "hello, world" {
		t.Fatalf("utf8 get: %s", got)
	}
	// Non-string JSON with utf8 stores its text; base64 insists on a string.
	raw = callBlob(t, blobPut, d, "t1", `{"name":"cfg/x.json","value":{"a":1},"encoding":"utf8"}`, "")
	wantOK(t, raw)
	raw = callBlob(t, blobPut, d, "t1", `{"name":"cfg/y.json","value":{"a":1}}`, "")
	wantCode(t, raw, "txco_blob_invalid_arg")
}

func TestBlobStatHitMiss(t *testing.T) {
	d := newBlobDeps(t, 0)
	miss := callBlob(t, blobStat, d, "t1", `{"name":"faqs/none.txt"}`, "")
	wantOK(t, miss)
	if gjson.Get(miss, "_blob.exists").Bool() || !gjson.Get(miss, "_blob.exists").Exists() {
		t.Fatalf("miss: %s", miss)
	}
	callBlob(t, blobPut, d, "t1", `{"name":"faqs/a.txt","value":"aGk=","filename":"A.txt","permissions":["blob:guest:read"],"grants":["blob:guest:read"]}`, "")
	hit := callBlob(t, blobStat, d, "t1", `{"name":"faqs/a.txt","audience":"guest"}`, "")
	wantOK(t, hit)
	if !gjson.Get(hit, "_blob.exists").Bool() || gjson.Get(hit, "_blob.size").Int() != 2 ||
		gjson.Get(hit, "_blob.filename").String() != "A.txt" || gjson.Get(hit, "_blob.permissions.0").String() != "blob:guest:read" ||
		gjson.Get(hit, "_blob.updated_at").String() != "2026-08-22T12:00:00Z" {
		t.Fatalf("hit: %s", hit)
	}
}

func TestBlobReplaceKeepsHistory(t *testing.T) {
	d := newBlobDeps(t, 0)
	v1, v2 := []byte("faq v1"), []byte("faq v2 (edited)")
	r1 := callBlob(t, blobPut, d, "t1", `{"name":"faqs/faq.md","value":"`+b64(v1)+`"}`, "")
	r2 := callBlob(t, blobPut, d, "t1", `{"name":"faqs/faq.md","value":"`+b64(v2)+`"}`, "")
	wantOK(t, r2)
	if !gjson.Get(r2, "_blob.replaced").Bool() || gjson.Get(r2, "_blob.existed").Bool() {
		t.Fatalf("replace: %s", r2)
	}
	// Name now points at v2 …
	got := callBlob(t, blobGet, d, "t1", `{"name":"faqs/faq.md","encoding":"utf8"}`, "")
	if gjson.Get(got, "_blob.content").String() != string(v2) || gjson.Get(got, "_blob.sha256").String() != shaOf(v2) {
		t.Fatalf("name → v2: %s", got)
	}
	// … and v1 is still addressable by sha with the privileged grant.
	old := callBlob(t, blobGet, d, "t1", `{"sha256":"`+gjson.Get(r1, "_blob.sha256").String()+`","encoding":"utf8","grants":["blob:cas:read"]}`, "")
	wantOK(t, old)
	if gjson.Get(old, "_blob.content").String() != string(v1) || gjson.Get(old, "_blob.name").Exists() {
		t.Fatalf("by-sha v1: %s", old)
	}
	// Without blob:cas:read the by-sha door is closed.
	wantCode(t, callBlob(t, blobGet, d, "t1", `{"sha256":"`+shaOf(v1)+`","audience":"guest"}`, ""), "txco_blob_denied")
	// Same bytes again: existed, not replaced.
	r3 := callBlob(t, blobPut, d, "t1", `{"name":"faqs/faq.md","value":"`+b64(v2)+`"}`, "")
	if !gjson.Get(r3, "_blob.existed").Bool() || gjson.Get(r3, "_blob.replaced").Bool() {
		t.Fatalf("idempotent put: %s", r3)
	}
}

func TestBlobDerivedNameDragAndDrop(t *testing.T) {
	d := newBlobDeps(t, 0)
	nfd := "Café Menu (v2).pdf" // e + combining acute, as macOS Finder names it
	nfc := "Café Menu (v2).pdf"
	r1 := callBlob(t, blobPut, d, "t1", `{"under":"paris/docs","filename":"`+nfd+`","value":"`+b64([]byte("%PDF-1"))+`"}`, "")
	wantOK(t, r1)
	name := gjson.Get(r1, "_blob.name").String()
	if !strings.HasPrefix(name, "paris/docs/") || len(name) != len("paris/docs/")+64 {
		t.Fatalf("derived name: %q", name)
	}
	// Same title typed NFC, new bytes → REPLACE on the same name.
	r2 := callBlob(t, blobPut, d, "t1", `{"under":"paris/docs","filename":"`+nfc+`","value":"`+b64([]byte("%PDF-2"))+`"}`, "")
	if gjson.Get(r2, "_blob.name").String() != name || !gjson.Get(r2, "_blob.replaced").Bool() {
		t.Fatalf("NFC re-upload must repoint the same name: %s", r2)
	}
	// The verbatim filename rides as metadata (latest put wins).
	st := callBlob(t, blobStat, d, "t1", `{"name":"`+name+`"}`, "")
	if gjson.Get(st, "_blob.filename").String() != nfc || gjson.Get(st, "_blob.content_type").String() != "application/pdf" {
		t.Fatalf("filename metadata: %s", st)
	}
	// Any filename works — emoji, spaces, 200 bytes.
	long := strings.Repeat("x", 200) + " 🎉.bin"
	wantOK(t, callBlob(t, blobPut, d, "t1", `{"under":"paris/docs","filename":"`+long+`","value":"AA=="}`, ""))
	// Bad shapes.
	wantCode(t, callBlob(t, blobPut, d, "t1", `{"under":"paris/docs","value":"AA=="}`, ""), "txco_blob_invalid_arg")
	wantCode(t, callBlob(t, blobPut, d, "t1", `{"name":"a","under":"b","filename":"c","value":"AA=="}`, ""), "txco_blob_invalid_arg")
}

func TestBlobNamelessPut(t *testing.T) {
	d := newBlobDeps(t, 0)
	r1 := callBlob(t, blobPut, d, "t1", `{"value":"`+b64([]byte("projection output"))+`"}`, "")
	wantOK(t, r1)
	if gjson.Get(r1, "_blob.name").Exists() || gjson.Get(r1, "_blob.replaced").Exists() || gjson.Get(r1, "_blob.existed").Bool() {
		t.Fatalf("nameless put: %s", r1)
	}
	r2 := callBlob(t, blobPut, d, "t1", `{"value":"`+b64([]byte("projection output"))+`"}`, "")
	if !gjson.Get(r2, "_blob.existed").Bool() {
		t.Fatalf("second nameless put of same bytes: %s", r2)
	}
	// Readable by sha with the privileged grant; stat by sha has no name.
	st := callBlob(t, blobStat, d, "t1", `{"sha256":"`+gjson.Get(r1, "_blob.sha256").String()+`","grants":["blob:cas:read"]}`, "")
	if !gjson.Get(st, "_blob.exists").Bool() || gjson.Get(st, "_blob.name").Exists() {
		t.Fatalf("stat by sha: %s", st)
	}
}

func TestBlobDeleteUnlinksOnly(t *testing.T) {
	d := newBlobDeps(t, 0)
	r := callBlob(t, blobPut, d, "t1", `{"name":"faqs/a.txt","value":"aGk="}`, "")
	sha := gjson.Get(r, "_blob.sha256").String()
	del := callBlob(t, blobDelete, d, "t1", `{"name":"faqs/a.txt"}`, "")
	wantOK(t, del)
	if !gjson.Get(del, "_blob.deleted").Bool() {
		t.Fatalf("delete: %s", del)
	}
	if gjson.Get(callBlob(t, blobStat, d, "t1", `{"name":"faqs/a.txt"}`, ""), "_blob.exists").Bool() {
		t.Fatal("name still resolves after delete")
	}
	if gjson.Get(callBlob(t, blobDelete, d, "t1", `{"name":"faqs/a.txt"}`, ""), "_blob.deleted").Bool() {
		t.Fatal("second delete reported deleted:true")
	}
	// Bytes stay: the tenant still owns the sha.
	if !gjson.Get(callBlob(t, blobStat, d, "t1", `{"sha256":"`+sha+`","grants":["blob:cas:read"]}`, ""), "_blob.exists").Bool() {
		t.Fatal("bytes vanished with the name")
	}
}

func TestBlobList(t *testing.T) {
	d := newBlobDeps(t, 0)
	empty := callBlob(t, blobList, d, "t1", `{}`, "")
	wantOK(t, empty)
	if gjson.Get(empty, "_blob.names").Raw != "[]" || gjson.Get(empty, "_blob.count").Int() != 0 || gjson.Get(empty, "_blob.next").String() != "" {
		t.Fatalf("empty list: %s", empty)
	}
	for _, n := range []string{"faqs/b.md", "faqs/a.md", "bookings/2026.csv", "faqs/c.md"} {
		callBlob(t, blobPut, d, "t1", `{"name":"`+n+`","value":"aGk="}`, "")
	}
	callBlob(t, blobPut, d, "t1", `{"name":"internal/ops.md","value":"aGk=","permissions":["blob:internal:read"],"grants":["blob:internal:read"]}`, "")

	all := callBlob(t, blobList, d, "t1", `{}`, "")
	if gjson.Get(all, "_blob.count").Int() != 5 || gjson.Get(all, "_blob.names.0.name").String() != "bookings/2026.csv" {
		t.Fatalf("all: %s", all)
	}
	// Rows carry their permissions (always an array) — reads stay gated, the
	// listing is metadata only.
	if gjson.Get(all, "_blob.names.4.permissions.0").String() != "blob:internal:read" || gjson.Get(all, "_blob.names.0.permissions").Raw != "[]" {
		t.Fatalf("permissions on rows: %s", all)
	}
	pre := callBlob(t, blobList, d, "t1", `{"prefix":"faqs/","limit":2}`, "")
	if gjson.Get(pre, "_blob.count").Int() != 2 || gjson.Get(pre, "_blob.names.0.name").String() != "faqs/a.md" ||
		gjson.Get(pre, "_blob.next").String() != "faqs/b.md" {
		t.Fatalf("prefix page 1: %s", pre)
	}
	pre2 := callBlob(t, blobList, d, "t1", `{"prefix":"faqs/","limit":2,"after":"faqs/b.md"}`, "")
	if gjson.Get(pre2, "_blob.count").Int() != 1 || gjson.Get(pre2, "_blob.names.0.name").String() != "faqs/c.md" || gjson.Get(pre2, "_blob.next").String() != "" {
		t.Fatalf("prefix page 2: %s", pre2)
	}
}

func TestBlobPermissions(t *testing.T) {
	d := newBlobDeps(t, 0)
	// An op cannot mint a name requiring what it does not hold.
	wantCode(t, callBlob(t, blobPut, d, "t1",
		`{"name":"internal/ops.md","value":"aGk=","permissions":["blob:internal:read"],"audience":"guest"}`, ""), "txco_blob_denied")
	wantCode(t, callBlob(t, blobPut, d, "t1",
		`{"name":"internal/ops.md","value":"aGk=","permissions":["blob:*:read"],"grants":["blob:*:*"]}`, ""), "txco_blob_invalid_arg")
	// The pipeline (wildcard grants) mints it.
	wantOK(t, callBlob(t, blobPut, d, "t1",
		`{"name":"internal/ops.md","value":"`+b64([]byte("secret ops"))+`","permissions":["blob:internal:read"],"grants":["blob:*:*"]}`, ""))
	// Same bytes, a guest-safe name: allowed via guest/, denied via internal/.
	wantOK(t, callBlob(t, blobPut, d, "t1",
		`{"name":"guest/ops.md","value":"`+b64([]byte("secret ops"))+`","permissions":["blob:guest:read"],"grants":["blob:*:*"]}`, ""))
	guestOK := callBlob(t, blobGet, d, "t1", `{"name":"guest/ops.md","audience":"guest","encoding":"utf8"}`, "")
	wantOK(t, guestOK)
	if gjson.Get(guestOK, "_blob.content").String() != "secret ops" {
		t.Fatalf("guest read of guest name: %s", guestOK)
	}
	denied := callBlob(t, blobGet, d, "t1", `{"name":"internal/ops.md","audience":"guest"}`, "")
	wantCode(t, denied, "txco_blob_denied")
	if gjson.Get(denied, "_blob.content").Exists() {
		t.Fatal("denied read leaked content")
	}
	// stat / delete / repoint honor the same requirement.
	wantCode(t, callBlob(t, blobStat, d, "t1", `{"name":"internal/ops.md","audience":"guest"}`, ""), "txco_blob_denied")
	wantCode(t, callBlob(t, blobDelete, d, "t1", `{"name":"internal/ops.md","audience":"guest"}`, ""), "txco_blob_denied")
	wantCode(t, callBlob(t, blobPut, d, "t1", `{"name":"internal/ops.md","value":"AA==","audience":"guest"}`, ""), "txco_blob_denied")
	// A holder repointing WITHOUT a permissions param keeps them (never a
	// silent declassify); an explicit [] clears them.
	wantOK(t, callBlob(t, blobPut, d, "t1", `{"name":"internal/ops.md","value":"AA==","audience":"internal"}`, ""))
	st := callBlob(t, blobStat, d, "t1", `{"name":"internal/ops.md","audience":"internal"}`, "")
	if gjson.Get(st, "_blob.permissions.0").String() != "blob:internal:read" {
		t.Fatalf("omitted permissions must be preserved: %s", st)
	}
	wantOK(t, callBlob(t, blobPut, d, "t1", `{"name":"internal/ops.md","value":"AA==","permissions":[],"audience":"internal"}`, ""))
	if gjson.Get(callBlob(t, blobStat, d, "t1", `{"name":"internal/ops.md","audience":"guest"}`, ""), "_blob.permissions").Raw != "[]" {
		t.Fatal("explicit [] did not clear permissions")
	}
	// Names without permissions are open within the tenant.
	wantOK(t, callBlob(t, blobPut, d, "t1", `{"name":"faqs/open.md","value":"AA=="}`, ""))
	wantOK(t, callBlob(t, blobGet, d, "t1", `{"name":"faqs/open.md","audience":"guest"}`, ""))
	// Malformed subject.
	wantCode(t, callBlob(t, blobGet, d, "t1", `{"name":"faqs/open.md","audience":"gu est"}`, ""), "txco_blob_invalid_arg")
}

func TestBlobTenantIsolationBothLayers(t *testing.T) {
	d := newBlobDeps(t, 0)
	r := callBlob(t, blobPut, d, "alpha", `{"name":"docs/secret.txt","value":"`+b64([]byte("alpha only"))+`"}`, "")
	sha := gjson.Get(r, "_blob.sha256").String()
	// Name layer.
	if gjson.Get(callBlob(t, blobStat, d, "beta", `{"name":"docs/secret.txt"}`, ""), "_blob.exists").Bool() {
		t.Fatal("name visible across tenants")
	}
	// Sha layer: beta holds blob:cas:read, and the bytes ARE in the shared
	// CAS — but beta never owned them, so the fence says not_found.
	wantCode(t, callBlob(t, blobGet, d, "beta", `{"sha256":"`+sha+`","grants":["blob:cas:read"]}`, ""), "txco_blob_not_found")
	if gjson.Get(callBlob(t, blobStat, d, "beta", `{"sha256":"`+sha+`","grants":["blob:cas:read"]}`, ""), "_blob.exists").Bool() {
		t.Fatal("sha ownership leaked")
	}
	// And beta putting the same bytes sees existed:false (no oracle).
	rb := callBlob(t, blobPut, d, "beta", `{"value":"`+b64([]byte("alpha only"))+`"}`, "")
	if gjson.Get(rb, "_blob.existed").Bool() {
		t.Fatal("existed leaked another tenant's holdings")
	}
	if gjson.Get(callBlob(t, blobList, d, "beta", `{}`, ""), "_blob.count").Int() != 0 {
		t.Fatal("list leaked across tenants")
	}
}

func TestBlobTooLarge(t *testing.T) {
	d := newBlobDeps(t, 4)
	wantCode(t, callBlob(t, blobPut, d, "t1", `{"name":"a/b","value":"`+b64([]byte("12345"))+`"}`, ""), "txco_blob_too_large")
	wantOK(t, callBlob(t, blobPut, d, "t1", `{"name":"a/b","value":"`+b64([]byte("1234"))+`"}`, ""))
	// get honors a per-op max_bytes under the cap.
	wantCode(t, callBlob(t, blobGet, d, "t1", `{"name":"a/b","max_bytes":3}`, ""), "txco_blob_too_large")
	wantOK(t, callBlob(t, blobGet, d, "t1", `{"name":"a/b","max_bytes":4}`, ""))
	// Nothing was stored for the oversize put.
	if gjson.Get(callBlob(t, blobStat, d, "t1", `{"sha256":"`+shaOf([]byte("12345"))+`","grants":["blob:cas:read"]}`, ""), "_blob.exists").Bool() {
		t.Fatal("oversize bytes were stored")
	}
}

func TestBlobInvalidArgsAndMisses(t *testing.T) {
	d := newBlobDeps(t, 0)
	wantCode(t, callBlob(t, blobGet, d, "t1", `{"name":"a/b","sha256":"`+shaOf(nil)+`"}`, ""), "txco_blob_invalid_arg")
	wantCode(t, callBlob(t, blobGet, d, "t1", `{}`, ""), "txco_blob_invalid_arg")
	wantCode(t, callBlob(t, blobGet, d, "t1", `{"name":"../x"}`, ""), "txco_blob_invalid_name")
	wantCode(t, callBlob(t, blobGet, d, "t1", `{"sha256":"nothex"}`, ""), "txco_blob_invalid_arg")
	wantCode(t, callBlob(t, blobGet, d, "t1", `{"name":"a/b","encoding":"hex"}`, ""), "txco_blob_invalid_arg")
	wantCode(t, callBlob(t, blobGet, d, "t1", `{"name":"a/missing"}`, ""), "txco_blob_not_found")
	wantCode(t, callBlob(t, blobPut, d, "t1", `{"name":"a/b"}`, ""), "txco_blob_invalid_arg")
	wantCode(t, callBlob(t, blobPut, d, "t1", `{"name":"a/b","from":"._nope"}`, ""), "txco_blob_invalid_arg")
	wantCode(t, callBlob(t, blobPut, d, "t1", `{"name":"a/b","value":"not base64!"}`, ""), "txco_blob_invalid_arg")
	wantCode(t, callBlob(t, blobPut, d, "t1", `{"name":"_meta/x","value":"AA=="}`, ""), "txco_blob_invalid_name")
	wantCode(t, callBlob(t, blobDelete, d, "t1", `{}`, ""), "txco_blob_invalid_arg")
}

func TestBlobNoTenantAndDisabled(t *testing.T) {
	d := newBlobDeps(t, 0)
	for _, fn := range []blobHandler{blobPut, blobGet, blobStat, blobList, blobDelete} {
		wantCode(t, callBlob(t, fn, d, "", `{"name":"a/b","value":"AA=="}`, ""), "txco_blob_no_tenant")
	}
	off := blobDeps{ix: d.ix}
	wantCode(t, callBlob(t, blobStat, off, "t1", `{"name":"a/b"}`, ""), "txco_blob_disabled")
}

func TestKVScopeRefusesReservedNamespace(t *testing.T) {
	k := newKVHandle(t)
	if _, err := callKV(t, kvSet, k, "t1", "hello", `{"key":"n:x","value":1,"namespace":"_txc.blob"}`, ""); err == nil {
		t.Fatal("kv/set into the blob index namespace was allowed")
	}
	if _, err := callKV(t, kvList, k, "t1", "hello", `{"namespace":"_txc.blob.sha"}`, ""); err == nil {
		t.Fatal("kv/list of a reserved namespace was allowed")
	}
	// A stack literally routed as _txc… can't use the default namespace either.
	if _, err := callKV(t, kvGet, k, "t1", "_txc.x", `{"key":"k"}`, ""); err == nil {
		t.Fatal("reserved routed-stack namespace allowed")
	}
	// Ordinary namespaces unaffected.
	if _, err := callKV(t, kvSet, k, "t1", "hello", `{"key":"k","value":1,"namespace":"_txcfoo"}`, ""); err != nil {
		t.Fatalf("non-reserved lookalike refused: %v", err)
	}
}

// TestBlobPutKeepsSeedBookkeeping — a runtime repoint of a pack-seeded name
// is DRIFT: seeded_by / seeded_sha survive so `txco data apply` can see it.
func TestBlobPutKeepsSeedBookkeeping(t *testing.T) {
	d := newBlobDeps(t, 0)
	seedSha := shaOf([]byte("hello"))
	if err := d.fcas.Put(context.Background(), seedSha, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := d.ix.PutName(context.Background(), "t1", blob.NameRow{Name: "hello.txt", SHA256: seedSha, Size: 5,
		SeededBy: "demo", SeededSHA: seedSha, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	wantOK(t, callBlob(t, blobPut, d, "t1", `{"name":"hello.txt","value":"`+b64([]byte("hola"))+`"}`, ""))
	row, _, _ := d.ix.GetName(context.Background(), "t1", "hello.txt")
	if row.SHA256 != shaOf([]byte("hola")) || row.SeededBy != "demo" || row.SeededSHA != seedSha || !row.Drifted() {
		t.Fatalf("runtime put must keep pack bookkeeping and read as drift: %+v", row)
	}
}
