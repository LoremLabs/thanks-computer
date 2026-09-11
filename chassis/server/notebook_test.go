package server

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	chnotebook "github.com/loremlabs/thanks-computer/chassis/notebook"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The notebook handler tests call the handlers the way ExecCore does:
// trusted tenant on ctx + WITH meta + an envelope carrying the routed
// stack, against the bundled sqlite backend in t.TempDir.

func newNotebookDeps(t *testing.T) notebookDeps {
	t.Helper()
	st, err := chnotebook.Open("sqlite", chnotebook.Config{DBPath: filepath.Join(t.TempDir(), "notebook.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return notebookDeps{store: st, maxExportBytes: 1 << 20}
}

type notebookHandler func(context.Context, notebookDeps, []byte) (event.Payload, error)

// callNotebook runs a handler for tenant with the given WITH meta. The
// envelope carries `_txc.op` (the fuel stage) and `_txc.route.stack`
// ("www" — the default namespace) plus inExtra merged in. A Go error from
// a handler fails the test: notebook errors must be envelope-surfaced.
func callNotebook(t *testing.T, fn notebookHandler, d notebookDeps, tenant, metaJSON, inExtra string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	in := `{"_txc":{"op":"demo/100/notebook","route":{"stack":"www"}}}`
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
		t.Fatalf("notebook handler returned a Go error (must be envelope-surfaced): %v", err)
	}
	return pl.Raw
}

func nbWantCode(t *testing.T, raw, code string) {
	t.Helper()
	if got := gjson.Get(raw, "_notebook.error.code").String(); got != code {
		t.Fatalf("_notebook.error.code = %q, want %q (raw %s)", got, code, raw)
	}
}

func nbWantOK(t *testing.T, raw string) {
	t.Helper()
	if e := gjson.Get(raw, "_notebook.error"); e.Exists() {
		t.Fatalf("unexpected _notebook.error: %s", e.Raw)
	}
}

func nbAppend(t *testing.T, d notebookDeps, name, typ, key string) string {
	t.Helper()
	meta := `{"notebook":"` + name + `","type":"` + typ + `","data":{"k":"` + key + `"}`
	if key != "" {
		meta += `,"object_key":"` + key + `"`
	}
	meta += `}`
	raw := callNotebook(t, notebookAppend, d, "t1", meta, "")
	nbWantOK(t, raw)
	return raw
}

func nbSeqs(raw string) []int64 {
	var out []int64
	for _, e := range gjson.Get(raw, "_notebook.entries").Array() {
		out = append(out, e.Get("seq").Int())
	}
	return out
}

func TestNotebookAppendReadRoundTrip(t *testing.T) {
	d := newNotebookDeps(t)
	a := nbAppend(t, d, "task/42", "task.created", "")
	if gjson.Get(a, "_notebook.seq").Int() != 1 || gjson.Get(a, "_notebook.existed").Bool() {
		t.Fatalf("first append = %s", a)
	}
	if at := gjson.Get(a, "_notebook.at").String(); len(at) != 30 || !strings.HasSuffix(at, "Z") {
		t.Errorf("at = %q, want fixed-width UTC", at)
	}
	nbAppend(t, d, "task/42", "task.started", "")
	raw := callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/42","type":"note","data":"free text"}`, "")
	nbWantOK(t, raw)

	r := callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/42"}`, "")
	nbWantOK(t, r)
	if got := nbSeqs(r); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("seqs = %v", got)
	}
	if gjson.Get(r, "_notebook.count").Int() != 3 || gjson.Get(r, "_notebook.next").String() != "" ||
		gjson.Get(r, "_notebook.cursor").String() == "" || gjson.Get(r, "_notebook.truncated").Bool() {
		t.Errorf("read summary = %s", r)
	}
	entries := gjson.Get(r, "_notebook.entries").Array()
	if entries[0].Get("data.k").String() != "" || entries[0].Get("type").String() != "task.created" {
		t.Errorf("entry 1 = %s", entries[0].Raw)
	}
	if entries[2].Get("data").String() != "free text" {
		t.Errorf("scalar data lost: %s", entries[2].Raw)
	}
	if entries[0].Get("object_key").Exists() {
		t.Errorf("empty object_key should be omitted: %s", entries[0].Raw)
	}
}

func TestNotebookDuplicateObjectKeyReturnsOriginal(t *testing.T) {
	d := newNotebookDeps(t)
	first := nbAppend(t, d, "task/1", "email.received", "recv:1")
	again := callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/1","type":"other","object_key":"recv:1","data":{"ignored":true}}`, "")
	nbWantOK(t, again)
	if !gjson.Get(again, "_notebook.existed").Bool() ||
		gjson.Get(again, "_notebook.seq").Int() != gjson.Get(first, "_notebook.seq").Int() ||
		gjson.Get(again, "_notebook.at").String() != gjson.Get(first, "_notebook.at").String() {
		t.Errorf("duplicate = %s, first = %s", again, first)
	}
	r := callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/1"}`, "")
	if gjson.Get(r, "_notebook.count").Int() != 1 || gjson.Get(r, "_notebook.entries.0.object_key").String() != "recv:1" {
		t.Errorf("read after duplicate = %s", r)
	}
}

func TestNotebookTailSinceAndDrain(t *testing.T) {
	d := newNotebookDeps(t)
	var ats []string
	for i := 0; i < 5; i++ {
		ats = append(ats, gjson.Get(nbAppend(t, d, "task/2", "t", ""), "_notebook.at").String())
	}
	r := callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/2","tail":2}`, "")
	if got := nbSeqs(r); len(got) != 2 || got[0] != 4 || got[1] != 5 || gjson.Get(r, "_notebook.next").String() != "" {
		t.Errorf("tail 2 = %s", r)
	}
	r = callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/2","since":"`+ats[3]+`"}`, "")
	if got := nbSeqs(r); len(got) != 2 || got[0] != 4 {
		t.Errorf("since = %v (%s)", got, r)
	}
	r = callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/2","since":"`+ats[1]+`","until":"`+ats[3]+`"}`, "")
	if got := nbSeqs(r); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("since/until = %v", got)
	}

	// The drain idiom: after = next until next == "".
	var all []int64
	passes := 0
	meta := `{"notebook":"task/2","limit":2}`
	for {
		r := callNotebook(t, notebookRead, d, "t1", meta, "")
		nbWantOK(t, r)
		passes++
		all = append(all, nbSeqs(r)...)
		next := gjson.Get(r, "_notebook.next").String()
		if next == "" {
			if gjson.Get(r, "_notebook.truncated").Bool() {
				t.Errorf("last page truncated: %s", r)
			}
			break
		}
		if next != gjson.Get(r, "_notebook.cursor").String() || !gjson.Get(r, "_notebook.truncated").Bool() {
			t.Errorf("full page: %s", r)
		}
		meta, _ = sjson.Set(meta, "after", next)
	}
	if passes != 3 || len(all) != 5 || all[4] != 5 {
		t.Errorf("drain: passes=%d all=%v", passes, all)
	}

	// A bare number is not a cursor.
	r = callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/2","after":3}`, "")
	nbWantCode(t, r, "txco_notebook_invalid_arg")
	r = callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/2","since":"yesterday"}`, "")
	nbWantCode(t, r, "txco_notebook_invalid_arg")
}

func TestNotebookExportNDJSON(t *testing.T) {
	d := newNotebookDeps(t)
	for i := 0; i < 3; i++ {
		nbAppend(t, d, "task/3", "t", "")
	}
	raw := callNotebook(t, notebookExport, d, "t1", `{"notebook":"task/3"}`, "")
	nbWantOK(t, raw)
	if gjson.Get(raw, "_txc.web.res.status").Int() != 200 ||
		gjson.Get(raw, "_txc.web.res.headers.content-type.0").String() != "application/x-ndjson" ||
		!gjson.Get(raw, "_txc.halt").Bool() {
		t.Fatalf("export envelope = %s", raw)
	}
	body, err := base64.StdEncoding.DecodeString(gjson.Get(raw, "_txc.web.res.body").String())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d: %q", len(lines), body)
	}
	for i, l := range lines {
		if !gjson.Valid(l) || gjson.Get(l, "seq").Int() != int64(i+1) {
			t.Errorf("line %d = %q", i, l)
		}
	}
	if gjson.Get(raw, "_notebook.count").Int() != 3 || gjson.Get(raw, "_notebook.truncated").Bool() ||
		gjson.Get(raw, "_notebook.next").String() != "" || gjson.Get(raw, "_notebook.cursor").String() == "" ||
		gjson.Get(raw, "_txc.web.res.headers.x-notebook-next").Exists() {
		t.Errorf("export summary = %s", raw)
	}

	// Row cap: truncated, resumable.
	raw = callNotebook(t, notebookExport, d, "t1", `{"notebook":"task/3","limit":2}`, "")
	next := gjson.Get(raw, "_notebook.next").String()
	if gjson.Get(raw, "_notebook.count").Int() != 2 || !gjson.Get(raw, "_notebook.truncated").Bool() || next == "" ||
		gjson.Get(raw, "_txc.web.res.headers.x-notebook-next.0").String() != next {
		t.Errorf("limited export = %s", raw)
	}
	raw = callNotebook(t, notebookExport, d, "t1", `{"notebook":"task/3","after":"`+next+`"}`, "")
	if gjson.Get(raw, "_notebook.count").Int() != 1 || gjson.Get(raw, "_notebook.truncated").Bool() {
		t.Errorf("resumed export = %s", raw)
	}
	// Byte cap: stops on a line boundary.
	small := d
	small.maxExportBytes = 80
	raw = callNotebook(t, notebookExport, small, "t1", `{"notebook":"task/3"}`, "")
	body, _ = base64.StdEncoding.DecodeString(gjson.Get(raw, "_txc.web.res.body").String())
	if !gjson.Get(raw, "_notebook.truncated").Bool() || len(body) > 80 || !strings.HasSuffix(string(body), "\n") {
		t.Errorf("byte-capped export = %s (body %q)", raw, body)
	}
	// Empty notebook: still a body, still NDJSON-safe.
	raw = callNotebook(t, notebookExport, d, "t1", `{"notebook":"task/none"}`, "")
	nbWantOK(t, raw)
	body, _ = base64.StdEncoding.DecodeString(gjson.Get(raw, "_txc.web.res.body").String())
	if string(body) != "\n" || gjson.Get(raw, "_notebook.count").Int() != 0 {
		t.Errorf("empty export = %s", raw)
	}
	raw = callNotebook(t, notebookExport, d, "t1", `{"notebook":"task/3","format":"csv"}`, "")
	nbWantCode(t, raw, "txco_notebook_invalid_arg")
}

func TestNotebookErrorsSurfaceOnEnvelope(t *testing.T) {
	d := newNotebookDeps(t)
	d.store.SetLimits(chnotebook.Limits{MaxDataBytes: 64})
	raw := callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/4","type":"t","data":{"blob":"`+strings.Repeat("x", 100)+`"}}`, "")
	nbWantCode(t, raw, "txco_notebook_too_large")
	raw = callNotebook(t, notebookAppend, d, "t1", `{"notebook":"_private/x","type":"t"}`, "")
	nbWantCode(t, raw, "txco_notebook_invalid_name")
	raw = callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/4"}`, "")
	nbWantCode(t, raw, "txco_notebook_invalid_arg")
	raw = callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/4","type":"t","ttl":-5}`, "")
	nbWantCode(t, raw, "txco_notebook_invalid_arg")
	raw = callNotebook(t, notebookAppend, d, "", `{"notebook":"task/4","type":"t"}`, "")
	nbWantCode(t, raw, "txco_notebook_no_tenant")
	raw = callNotebook(t, notebookAppend, notebookDeps{}, "t1", `{"notebook":"task/4","type":"t"}`, "")
	nbWantCode(t, raw, "txco_notebook_disabled")
	raw = callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/4","type":"t","namespace":"_txc.blob"}`, "")
	nbWantCode(t, raw, "txco_notebook_invalid_arg")
	// `into` is honoured for errors too.
	raw = callNotebook(t, notebookAppend, d, "t1", `{"notebook":"_x","type":"t","into":"_nb"}`, "")
	if gjson.Get(raw, "_nb.error.code").String() != "txco_notebook_invalid_name" {
		t.Errorf("into on error: %s", raw)
	}
	// And nothing above created a notebook.
	l := callNotebook(t, notebookList, d, "t1", `{}`, "")
	if gjson.Get(l, "_notebook.count").Int() != 0 {
		t.Errorf("rejected appends created notebooks: %s", l)
	}
}

func TestNotebookNamespaceScoping(t *testing.T) {
	d := newNotebookDeps(t)
	// Default namespace = the routed stack ("www").
	nbAppend(t, d, "task/5", "t", "")
	// An inlet sub-stack collapses to the app namespace.
	raw := callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/6","type":"t"}`, `{"_txc.route.stack":"www/_mail"}`)
	nbWantOK(t, raw)
	// An explicit namespace is its own space.
	raw = callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/7","type":"t","namespace":"pony-x"}`, "")
	nbWantOK(t, raw)
	// Another tenant sees none of it.
	raw = callNotebook(t, notebookList, d, "t2", `{}`, "")
	if gjson.Get(raw, "_notebook.count").Int() != 0 {
		t.Errorf("cross-tenant list = %s", raw)
	}

	l := callNotebook(t, notebookList, d, "t1", `{"prefix":"task/"}`, "")
	names := []string{}
	for _, n := range gjson.Get(l, "_notebook.notebooks").Array() {
		names = append(names, n.Get("name").String())
	}
	if strings.Join(names, ",") != "task/5,task/6" {
		t.Errorf("www notebooks = %v (%s)", names, l)
	}
	if gjson.Get(l, "_notebook.notebooks.0.high_seq").Int() != 1 || !gjson.Get(l, "_notebook.notebooks.0.ttl_secs").Exists() {
		t.Errorf("list row = %s", gjson.Get(l, "_notebook.notebooks.0").Raw)
	}
	l = callNotebook(t, notebookList, d, "t1", `{"namespace":"pony-x"}`, "")
	if gjson.Get(l, "_notebook.count").Int() != 1 || gjson.Get(l, "_notebook.notebooks.0.name").String() != "task/7" {
		t.Errorf("pony-x notebooks = %s", l)
	}
	// _txc.stack (the mail path) is read when _txc.route.stack is absent.
	raw = callNotebook(t, notebookAppend, d, "t1", `{"notebook":"task/8","type":"t"}`, `{"_txc.route.stack":"","_txc.stack":"core/_mail"}`)
	nbWantOK(t, raw)
	l = callNotebook(t, notebookList, d, "t1", `{"namespace":"core"}`, "")
	if gjson.Get(l, "_notebook.count").Int() != 1 {
		t.Errorf("core notebooks = %s", l)
	}
}

func TestNotebookDeleteAndStaleCursor(t *testing.T) {
	d := newNotebookDeps(t)
	nbAppend(t, d, "task/9", "t", "")
	nbAppend(t, d, "task/9", "t", "")
	r := callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/9"}`, "")
	cursor := gjson.Get(r, "_notebook.cursor").String()
	raw := callNotebook(t, notebookDelete, d, "t1", `{"notebook":"task/9"}`, "")
	nbWantOK(t, raw)
	if !gjson.Get(raw, "_notebook.deleted").Bool() {
		t.Errorf("delete = %s", raw)
	}
	raw = callNotebook(t, notebookDelete, d, "t1", `{"notebook":"task/9"}`, "")
	if gjson.Get(raw, "_notebook.deleted").Bool() {
		t.Errorf("second delete = %s", raw)
	}
	r = callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/9"}`, "")
	nbWantOK(t, r)
	if gjson.Get(r, "_notebook.count").Int() != 0 || !gjson.Get(r, "_notebook.entries").IsArray() {
		t.Errorf("read after delete = %s", r)
	}
	if a := nbAppend(t, d, "task/9", "t", ""); gjson.Get(a, "_notebook.seq").Int() != 1 {
		t.Errorf("recreate = %s", a)
	}
	r = callNotebook(t, notebookRead, d, "t1", `{"notebook":"task/9","after":"`+cursor+`"}`, "")
	nbWantCode(t, r, "txco_notebook_stale_cursor")
}
