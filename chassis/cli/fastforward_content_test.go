package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/state"
	"github.com/loremlabs/thanks-computer/chassis/stackdir"
)

func TestFFVerdict(t *testing.T) {
	m := func(kv ...string) map[string]string {
		out := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			out[kv[i]] = kv[i+1]
		}
		return out
	}
	cases := []struct {
		name              string
		base, active, loc map[string]string
		kind              ffKind
		edits, conflicts  []string
	}{
		{"in sync", m("a", "1"), m("a", "2"), m("a", "2"), ffInSync, nil, nil},
		{"in sync without a baseline", nil, m("a", "2"), m("a", "2"), ffInSync, nil, nil},
		{"forward: the tree has their change, its edit is elsewhere",
			m("a", "1", "b", "1"), m("a", "2", "b", "1"), m("a", "2", "b", "9"), ffForward, []string{"b"}, nil},
		{"conflict: they changed a, the tree still has the old a",
			m("a", "1", "b", "1"), m("a", "2", "b", "1"), m("a", "1", "b", "9"), ffConflict, []string{"a", "b"}, []string{"a"}},
		{"conflict: both changed a differently",
			m("a", "1"), m("a", "2"), m("a", "3"), ffConflict, []string{"a"}, []string{"a"}},
		{"they removed a file the tree removed too", m("a", "1", "b", "1"), m("a", "1"), m("a", "1", "c", "1"), ffForward, []string{"c"}, nil},
		{"they removed a file the tree still has", m("a", "1", "b", "1"), m("a", "1"), m("a", "1", "b", "1"), ffConflict, []string{"b"}, []string{"b"}},
		{"they added a file the tree lacks", m("a", "1"), m("a", "1", "n", "1"), m("a", "2"), ffConflict, []string{"a", "n"}, []string{"n"}},
		{"rollback the tree already took", m("a", "2"), m("a", "1"), m("a", "1", "b", "1"), ffForward, []string{"b"}, nil},
		{"no baseline, differs", nil, m("a", "1"), m("a", "2"), ffNoBase, []string{"a"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, edits, conflicts := ffVerdict(tc.base, tc.active, tc.loc)
			if kind != tc.kind || !reflect.DeepEqual(edits, tc.edits) || !reflect.DeepEqual(conflicts, tc.conflicts) {
				t.Fatalf("got kind=%d edits=%v conflicts=%v; want kind=%d edits=%v conflicts=%v", kind, edits, conflicts, tc.kind, tc.edits, tc.conflicts)
			}
		})
	}
}

// The comparison is code only: data packs deploy separately, and a COMPUTES/
// row's change already shows in the op that references it.
func TestCodeHashesSkipsPacksAndComputes(t *testing.T) {
	files := []client.StackFile{
		{Path: "100/x.txcl", Content: "EMIT .a = 1"},
		{Path: "FILES/a.css", ContentHash: "h"},
		{Path: "COMPUTES/" + strings.Repeat("a", 64) + ".json", ContentHash: "c"},
	}
	for _, p := range []string{"VECTORS/v.jsonl", "KV/k.jsonl", "BLOBS/b.md", "CALENDARS/u/c.jsonl", "CONTACTS/u/c.jsonl"} {
		if !isDataPackPath(p) {
			t.Errorf("%s should be a data pack path", p)
		}
		files = append(files, client.StackFile{Path: p, ContentHash: "d"})
	}
	got := codeHashes(files)
	if len(got) != 2 || got["FILES/a.css"] != "h" || got["100/x.txcl"] != fileHash(files[0]) {
		t.Fatalf("codeHashes = %v", got)
	}
}

// A STACKDIR/ bundle is one row for the stack's whole directory; when its
// digest differs it is opened, so the check sees which files changed.
func TestTreeReaderExpandsStackDir(t *testing.T) {
	dir := t.TempDir()
	pack := func(files map[string]string) string {
		var fs []stackdir.File
		for p, c := range files {
			fs = append(fs, stackdir.File{Path: p, Content: []byte(c)})
		}
		data, digest, err := stackdir.Pack(fs)
		if err != nil {
			t.Fatal(err)
		}
		cache := filepath.Join(dir, ".txco", "stackdir")
		_ = os.MkdirAll(cache, 0o755)
		if err := os.WriteFile(filepath.Join(cache, digest+stackdir.Ext), data, 0o644); err != nil {
			t.Fatal(err)
		}
		return stackdir.Path(digest)
	}
	baseRow := pack(map[string]string{"bin/a.sh": "1", "bin/b.sh": "1"})
	activeRow := pack(map[string]string{"bin/a.sh": "2", "bin/b.sh": "1"}) // they changed a
	localRow := pack(map[string]string{"bin/a.sh": "2", "bin/b.sh": "9"})  // the tree has it, and changed b
	base, active, local := map[string]string{baseRow: "x"}, map[string]string{activeRow: "y"}, map[string]string{localRow: "z"}
	tr := newTreeReader(t.Context(), nil, dir, nil)
	if err := tr.expand(base, active, local); err != nil {
		t.Fatal(err)
	}
	kind, edits, _ := ffVerdict(base, active, local)
	if kind != ffForward || !reflect.DeepEqual(edits, []string{"STACKDIR:bin/b.sh"}) {
		t.Fatalf("kind=%d edits=%v, want a fast-forward with the one local edit", kind, edits)
	}
	// The same bundle everywhere is never opened.
	same := map[string]string{"STACKDIR/" + strings.Repeat("0", 64) + ".tar": "q"}
	if err := tr.expand(same, map[string]string{"STACKDIR/" + strings.Repeat("0", 64) + ".tar": "q"}); err != nil {
		t.Fatalf("identical rows must not be read: %v", err)
	}
}

// versionedAdmin is an in-memory chassis admin with real versions: per-file
// hashes, an active pointer, the activate compare-and-swap. Enough of the API
// for push, sync and status.
type versionedAdmin struct {
	mu       sync.Mutex
	versions map[string][]map[string]string // stack → version n-1 → path → content
	active   map[string]int64
	drafts   int
}

func newVersionedAdmin(t *testing.T) (*versionedAdmin, *httptest.Server) {
	t.Helper()
	a := &versionedAdmin{versions: map[string][]map[string]string{}, active: map[string]int64{}}
	srv := httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(srv.Close)
	return a, srv
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func (a *versionedAdmin) files(stack string, n int64) []client.StackFile {
	var out []client.StackFile
	for p, c := range a.versions[stack][n-1] {
		out = append(out, client.StackFile{Path: p, ContentHash: sha(c)})
	}
	return out
}

// deploy mints and activates a version directly — someone deploying from
// another machine.
func (a *versionedAdmin) deploy(stack string, files map[string]string) int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.versions[stack] = append(a.versions[stack], files)
	n := int64(len(a.versions[stack]))
	a.active[stack] = n
	return n
}

func (a *versionedAdmin) activeFiles(stack string) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.versions[stack][a.active[stack]-1]
}

func (a *versionedAdmin) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	rest, ok := strings.CutPrefix(r.URL.Path, "/v1/tenants/default/stacks")
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	record := func(stack string) client.StackRecord {
		rec := client.StackRecord{Name: stack}
		if n := a.active[stack]; n > 0 {
			rec.ActiveVersion = &n
			rec.ManifestHash = localManifestHash(a.files(stack, n))
			rec.CodeManifestHash = rec.ManifestHash
		}
		return rec
	}
	switch {
	case r.Method == http.MethodGet && parts[0] == "":
		var recs []client.StackRecord
		for s := range a.versions {
			recs = append(recs, record(s))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"stacks": recs})
	case len(parts) == 1 && r.Method == http.MethodGet:
		if _, ok := a.versions[parts[0]]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"stack_not_found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(record(parts[0]))
	case len(parts) == 2 && parts[1] == "versions":
		var vs []client.VersionRecord
		for i := len(a.versions[parts[0]]); i >= 1; i-- {
			at := "now"
			vs = append(vs, client.VersionRecord{VersionNumber: int64(i), ActivatedAt: &at,
				ManifestHash: localManifestHash(a.files(parts[0], int64(i))), IsActive: int64(i) == a.active[parts[0]]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"versions": vs})
	case len(parts) == 3 && parts[1] == "versions" && r.Method == http.MethodGet:
		n, _ := strconv.ParseInt(parts[2], 10, 64)
		if n < 1 || n > int64(len(a.versions[parts[0]])) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"version_not_found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(client.VersionDetail{VersionRecord: client.VersionRecord{VersionNumber: n}, Files: a.files(parts[0], n)})
	case len(parts) == 2 && parts[1] == "draft":
		a.drafts++
		a.versions[parts[0]] = append(a.versions[parts[0]], map[string]string{})
		_ = json.NewEncoder(w).Encode(map[string]any{"version_number": len(a.versions[parts[0]])})
	case len(parts) == 4 && parts[3] == "files":
		n, _ := strconv.ParseInt(parts[2], 10, 64)
		var req struct {
			Files []client.StackFile `json:"files"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		files := map[string]string{}
		for _, f := range req.Files {
			files[f.Path] = f.Content
		}
		a.versions[parts[0]][n-1] = files
		_ = json.NewEncoder(w).Encode(map[string]any{"manifest_hash": localManifestHash(a.files(parts[0], n))})
	case len(parts) == 4 && parts[3] == "validate":
		_, _ = w.Write([]byte(`{"ok":true}`))
	case len(parts) == 2 && parts[1] == "activate":
		var req struct {
			VersionNumber  int64  `json:"version_number"`
			ExpectedActive *int64 `json:"expected_active"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		prior := a.active[parts[0]]
		if req.ExpectedActive != nil && *req.ExpectedActive != prior {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"stack_moved"}`))
			return
		}
		a.active[parts[0]] = req.VersionNumber
		_ = json.NewEncoder(w).Encode(map[string]any{"version_number": req.VersionNumber, "prior_version_number": prior})
	case len(parts) == 2 && parts[1] == "blobs":
		_ = json.NewEncoder(w).Encode(map[string]any{"stack": parts[0], "names": []any{}, "count": 0})
	default:
		http.NotFound(w, r)
	}
}

// writeOp writes one op under a labelled step directory, the way a real
// tree is laid out (the chassis stores it as <scope>/<name>.txcl).
func writeOp(t *testing.T, root, stack, step, name, txcl string) {
	t.Helper()
	d := filepath.Join(root, "OPS", stack, step)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, name+".txcl"), []byte(txcl), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cloneTree copies a workspace's OPS/ tree — and its .txco/ only when
// withState — into a new directory: another machine's checkout.
func cloneTree(t *testing.T, from string, withState bool) string {
	t.Helper()
	to := t.TempDir()
	for _, sub := range []string{"OPS", state.Dir} {
		if sub == state.Dir && !withState {
			continue
		}
		src := filepath.Join(from, sub)
		err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(from, p)
			if info.IsDir() {
				return os.MkdirAll(filepath.Join(to, rel), 0o755)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(to, rel), b, 0o644)
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	return to
}

func pushOK(t *testing.T, root, url string) string {
	t.Helper()
	var out, errb bytes.Buffer
	if code := runPush([]string{"api", root, "--addr", url, "--yes"}, &out, &errb); code != 0 {
		t.Fatalf("push exit=%d; stderr=%s", code, errb.String())
	}
	return out.String() + errb.String()
}

func baseline(t *testing.T, root, url string) *state.State {
	t.Helper()
	s, err := state.Load(root, "api", state.Key(url, "default"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Machine A deploys; machine B, whose baseline is older, pulls the change
// in with git and edits another file: its push fast-forwards and deploys.
func TestPushFastForwardsStaleBaseline(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	adm, srv := newVersionedAdmin(t)
	a := t.TempDir()
	writeOp(t, a, "api", "0100_GREET", "hello", `EMIT .hello = "1"`)
	writeOp(t, a, "api", "0200_REPLY", "reply", `EMIT .reply = "1"`)
	pushOK(t, a, srv.URL) // v1
	b := cloneTree(t, a, true)

	writeOp(t, a, "api", "0100_GREET", "hello", `EMIT .hello = "2"`)
	pushOK(t, a, srv.URL) // v2, from machine A

	writeOp(t, b, "api", "0100_GREET", "hello", `EMIT .hello = "2"`) // git pull
	writeOp(t, b, "api", "0200_REPLY", "reply", `EMIT .reply = "B"`) // B's own edit
	out := pushOK(t, b, srv.URL)
	if !strings.Contains(out, "baseline v1 → v2") || !strings.Contains(out, "1 local edit") {
		t.Fatalf("push output lacks the fast-forward line:\n%s", out)
	}
	if got := adm.activeFiles("api")["200/reply.txcl"]; got != `EMIT .reply = "B"` {
		t.Fatalf("active reply = %q, want B's edit deployed", got)
	}
	if got := adm.activeFiles("api")["100/hello.txcl"]; got != `EMIT .hello = "2"` {
		t.Fatalf("active hello = %q, want A's change kept", got)
	}
	if s := baseline(t, b, srv.URL); s == nil || s.VersionNumber != 3 || s.TreeHash == "" {
		t.Fatalf("B's baseline = %+v, want v3 with a tree hash", s)
	}
}

// Machine B never brought A's change in: its push would drop it. Refused,
// naming the file as the author wrote it.
func TestPushRefusesWhenTreeLacksTheirChange(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	adm, srv := newVersionedAdmin(t)
	a := t.TempDir()
	writeOp(t, a, "api", "0100_GREET", "hello", `EMIT .hello = "1"`)
	writeOp(t, a, "api", "0200_REPLY", "reply", `EMIT .reply = "1"`)
	pushOK(t, a, srv.URL)
	b := cloneTree(t, a, true)
	writeOp(t, a, "api", "0100_GREET", "hello", `EMIT .hello = "2"`)
	pushOK(t, a, srv.URL)

	writeOp(t, b, "api", "0200_REPLY", "reply", `EMIT .reply = "B"`)
	var out, errb bytes.Buffer
	if code := runPush([]string{"api", b, "--addr", srv.URL, "--yes"}, &out, &errb); code == 0 {
		t.Fatalf("push of a tree without A's change succeeded:\n%s", out.String())
	}
	msg := errb.String()
	for _, want := range []string{"refused", "from v1", "to v2", "OPS/api/0100_GREET/hello.txcl", "`txco push api --force`", "refused (the chassis moved)"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "failed after retries") {
		t.Fatalf("a refusal is not a retry failure:\n%s", msg)
	}
	if adm.drafts != 2 {
		t.Fatalf("drafts = %d, want 2 (the refused push must not draft)", adm.drafts)
	}
	// --force overwrites the chassis.
	if code := runPush([]string{"api", b, "--addr", srv.URL, "--yes", "--force"}, &out, &errb); code != 0 {
		t.Fatalf("push --force exit=%d; stderr=%s", code, errb.String())
	}
}

// A fresh checkout whose tree is what's deployed: push is a no-op that
// records the baseline, and status then reads in sync and clean.
func TestPushRecordsBaselineForFreshCheckout(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	_, srv := newVersionedAdmin(t)
	a := t.TempDir()
	writeOp(t, a, "api", "0100_GREET", "hello", `EMIT .hello = "1"`)
	pushOK(t, a, srv.URL)
	b := cloneTree(t, a, false)
	pushOK(t, b, srv.URL)
	if s := baseline(t, b, srv.URL); s == nil || s.VersionNumber != 1 || s.TreeHash == "" {
		t.Fatalf("fresh checkout's baseline = %+v, want v1 recorded", s)
	}
	var out, errb bytes.Buffer
	if code := runStatus([]string{b, "--addr", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("status exit=%d:\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "(clean)") || !strings.Contains(out.String(), "in sync") {
		t.Fatalf("status after the recorded baseline:\n%s", out.String())
	}
}

// A fresh checkout that is exactly an older version: pushing it would undo
// the newer one. Refused, and the older version recorded.
func TestPushRefusesBehindTree(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	adm, srv := newVersionedAdmin(t)
	a := t.TempDir()
	writeOp(t, a, "api", "0100_GREET", "hello", `EMIT .hello = "1"`)
	pushOK(t, a, srv.URL)
	b := cloneTree(t, a, false)
	writeOp(t, a, "api", "0100_GREET", "hello", `EMIT .hello = "2"`)
	pushOK(t, a, srv.URL)

	var out, errb bytes.Buffer
	if code := runPush([]string{"api", b, "--addr", srv.URL, "--yes"}, &out, &errb); code == 0 {
		t.Fatal("push of an older tree succeeded")
	}
	if !strings.Contains(errb.String(), "this tree is v1 exactly") {
		t.Fatalf("refusal:\n%s", errb.String())
	}
	if s := baseline(t, b, srv.URL); s == nil || s.VersionNumber != 1 {
		t.Fatalf("baseline = %+v, want v1 recorded", s)
	}
	if adm.active["api"] != 2 {
		t.Fatalf("active = v%d, want v2 untouched", adm.active["api"])
	}
}

func TestSyncVerdicts(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	adm, srv := newVersionedAdmin(t)
	a := t.TempDir()
	writeOp(t, a, "api", "0100_GREET", "hello", `EMIT .hello = "1"`)
	pushOK(t, a, srv.URL)
	sync := func(root string, extra ...string) (int, []syncRow) {
		t.Helper()
		var out, errb bytes.Buffer
		code := runSync(append([]string{root, "--addr", srv.URL, "--json"}, extra...), &out, &errb)
		var rows []syncRow
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatalf("sync --json: %v\n%s\n%s", err, out.String(), errb.String())
		}
		return code, rows
	}

	// A fresh checkout of what's deployed: in sync, recorded.
	b := cloneTree(t, a, false)
	if code, rows := sync(b); code != 0 || rows[0].Verdict != "in_sync" || rows[0].After != 1 {
		t.Fatalf("fresh in-sync checkout: code=%d rows=%+v", code, rows)
	}

	// A fresh checkout with its own edit: no baseline until adopted.
	c := cloneTree(t, a, false)
	writeOp(t, c, "api", "0100_GREET", "hello", `EMIT .hello = "C"`)
	if code, rows := sync(c); code != 1 || rows[0].Verdict != "no_baseline" || rows[0].Paths[0] != "OPS/api/0100_GREET/hello.txcl" {
		t.Fatalf("no baseline: code=%d rows=%+v", code, rows)
	}
	if code, rows := sync(c, "--adopt"); code != 0 || rows[0].Verdict != "adopted" || baseline(t, c, srv.URL).VersionNumber != 1 {
		t.Fatalf("adopt: code=%d rows=%+v", code, rows)
	}

	// Someone deploys v2 elsewhere; b brings it in (git pull) and syncs.
	adm.deploy("api", map[string]string{"100/hello.txcl": `EMIT .hello = "2"`})
	writeOp(t, b, "api", "0100_GREET", "hello", `EMIT .hello = "2"`)
	writeOp(t, b, "api", "0300_MORE", "more", `EMIT .more = "b"`)
	if code, rows := sync(b); code != 0 || rows[0].Verdict != "forwarded" || rows[0].Before != 1 || rows[0].After != 2 || rows[0].LocalEdits != 1 {
		t.Fatalf("forward: code=%d rows=%+v", code, rows)
	}
	// The baseline moved, but the recorded manifest is v2's: --changed still
	// sees b's edit as a change.
	if s := baseline(t, b, srv.URL); s.ManifestHash != localManifestHash(adm.files("api", 2)) {
		t.Fatalf("forwarded baseline manifest = %q, want v2's", s.ManifestHash)
	}

	// c adopted v1 and edited the same file v2 changed: a conflict.
	if code, rows := sync(c); code != 1 || rows[0].Verdict != "conflict" || len(rows[0].Paths) != 1 {
		t.Fatalf("conflict: code=%d rows=%+v", code, rows)
	}

	// A stack the chassis doesn't have yet.
	writeOp(t, b, "fresh", "0100", "x", `EMIT .x = 1`)
	_, rows := sync(b)
	found := false
	for _, r := range rows {
		if r.Stack == "fresh" {
			found = r.Verdict == "not_on_chassis"
		}
	}
	if !found {
		t.Fatalf("a local-only stack: rows=%+v", rows)
	}
}
