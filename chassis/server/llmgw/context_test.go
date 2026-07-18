package llmgw

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func ctxItems(t *testing.T, raw string) gjson.Result {
	t.Helper()
	r := gjson.Parse(raw)
	if !r.IsArray() {
		t.Fatalf("test items must be a JSON array: %s", raw)
	}
	return r
}

func statuses(rows []contextResultItem) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Status
	}
	return out
}

// systemTexts extracts the text of every system block from the request.
func systemTexts(t *testing.T, request string) []string {
	t.Helper()
	sys := gjson.Get(request, "system")
	if !sys.IsArray() {
		t.Fatalf("system is not an array: %s", sys.Raw)
	}
	var out []string
	for _, el := range sys.Array() {
		if el.Get("type").String() != "text" {
			t.Fatalf("non-text system block: %s", el.Raw)
		}
		out = append(out, el.Get("text").String())
	}
	return out
}

// TestInjectContextStringSystem: a string system is normalized to a
// one-block array, then guard, then the delimited items — in order.
func TestInjectContextStringSystem(t *testing.T) {
	req := `{"model":"m","system":"be terse","messages":[]}`
	items := ctxItems(t, `[{"source":"kv:decisions/adr-001","title":"Why BoltDB","content":"we chose boltdb"}]`)
	out, rows := injectContext(req, items, 2000, 8)

	texts := systemTexts(t, out)
	if len(texts) != 3 {
		t.Fatalf("system blocks = %d, want 3 (original, guard, item)", len(texts))
	}
	if texts[0] != "be terse" {
		t.Errorf("original system lost: %q", texts[0])
	}
	if texts[1] != contextGuardText {
		t.Errorf("guard block missing/mangled: %q", texts[1])
	}
	block := texts[2]
	for _, want := range []string{
		"--- BEGIN TXCO CONTEXT ---",
		"Source: kv:decisions/adr-001",
		"Title: Why BoltDB",
		"Length: 15 bytes",
		"we chose boltdb",
		"--- END TXCO CONTEXT ---",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("block missing %q:\n%s", want, block)
		}
	}

	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (guard + item)", len(rows))
	}
	if !rows[0].Synthetic || rows[0].Source != "txco:system_guard" || rows[0].Status != ctxInjected {
		t.Errorf("guard row = %+v", rows[0])
	}
	sum := sha256.Sum256([]byte("we chose boltdb"))
	if rows[1].SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("item sha256 = %q", rows[1].SHA256)
	}
	if rows[1].Bytes != 15 || rows[1].EstTokens != 4 || rows[1].Status != ctxInjected {
		t.Errorf("item row = %+v", rows[1])
	}
	// The summary must never carry content.
	for _, r := range rows {
		if strings.Contains(r.Source+r.Title, "we chose boltdb") {
			t.Errorf("summary leaked content: %+v", r)
		}
	}
}

// TestInjectContextArrayAndAbsentSystem: array systems append; absent
// systems are created.
func TestInjectContextArrayAndAbsentSystem(t *testing.T) {
	items := ctxItems(t, `[{"content":"note"}]`)

	out, _ := injectContext(`{"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, items, 2000, 8)
	texts := systemTexts(t, out)
	if len(texts) != 4 || texts[0] != "a" || texts[1] != "b" || texts[2] != contextGuardText {
		t.Errorf("array append wrong: %q", texts)
	}

	out, _ = injectContext(`{"model":"m"}`, items, 2000, 8)
	texts = systemTexts(t, out)
	if len(texts) != 2 || texts[0] != contextGuardText {
		t.Errorf("absent-system creation wrong: %q", texts)
	}
	// Headers for a label-less item: no Source/Title lines.
	if strings.Contains(texts[1], "Source:") || strings.Contains(texts[1], "Title:") {
		t.Errorf("label-less item grew header lines:\n%s", texts[1])
	}
}

// TestInjectContextBadSystemType: a non-string non-array system makes
// every row invalid and leaves the request untouched.
func TestInjectContextBadSystemType(t *testing.T) {
	req := `{"system":{"weird":true}}`
	out, rows := injectContext(req, ctxItems(t, `[{"content":"x"},{"content":"y"}]`), 2000, 8)
	if out != req {
		t.Errorf("request mutated despite bad system type")
	}
	if got := statuses(rows); len(got) != 2 || got[0] != ctxInvalid || got[1] != ctxInvalid {
		t.Errorf("statuses = %v, want all invalid", got)
	}
}

// TestInjectContextBudgetFirstFits: a too-big item drops but later
// smaller ones still fit; the item cap binds independently.
func TestInjectContextBudgetFirstFits(t *testing.T) {
	big := strings.Repeat("x", 4000)   // est 1000 tokens
	small := strings.Repeat("y", 400)  // est 100
	small2 := strings.Repeat("z", 400) // est 100
	items := ctxItems(t, `[{"content":"`+big+`"},{"content":"`+small+`"},{"content":"`+small2+`"}]`)

	// Budget 250: guard (~39) + 100 + 100 fit; the 1000-token item never does.
	out, rows := injectContext(`{}`, items, 250, 8)
	if got := statuses(rows); got[0] != ctxInjected /*guard*/ ||
		got[1] != ctxDroppedBudget || got[2] != ctxInjected || got[3] != ctxInjected {
		t.Fatalf("statuses = %v", got)
	}
	if texts := systemTexts(t, out); len(texts) != 3 {
		t.Errorf("blocks = %d, want guard + 2 items", len(texts))
	}

	// Item cap 1: second small item drops.
	_, rows = injectContext(`{}`, items, 250, 1)
	if got := statuses(rows); got[2] != ctxInjected || got[3] != ctxDroppedBudget {
		t.Errorf("item-cap statuses = %v", got)
	}
}

// TestInjectContextNeverBareGuard: when nothing fits the budget, the
// guard is not injected either and the request is untouched.
func TestInjectContextNeverBareGuard(t *testing.T) {
	req := `{"system":"s"}`
	items := ctxItems(t, `[{"content":"`+strings.Repeat("x", 4000)+`"}]`)
	out, rows := injectContext(req, items, 100, 8) // guard fits, item never does
	if out != req {
		t.Errorf("request mutated with nothing injectable")
	}
	if len(rows) != 1 || rows[0].Status != ctxDroppedBudget || rows[0].Synthetic {
		t.Errorf("rows = %+v, want single dropped_budget item row, no guard", rows)
	}
}

// TestInjectContextDedup: both axes — content hash, and (source,title)
// label pair; label-less distinct items both pass.
func TestInjectContextDedup(t *testing.T) {
	items := ctxItems(t, `[
		{"source":"a","title":"t","content":"same"},
		{"source":"b","title":"u","content":"same"},
		{"source":"a","title":"t","content":"different"},
		{"content":"one"},
		{"content":"two"}]`)
	_, rows := injectContext(`{}`, items, 2000, 8)
	got := statuses(rows)
	want := []string{ctxInjected /*guard*/, ctxInjected, ctxDroppedDup /*hash*/, ctxDroppedDup /*label*/, ctxInjected, ctxInjected}
	if len(got) != len(want) {
		t.Fatalf("rows = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
}

// TestInjectContextInvalidItems: non-objects and bad content are
// per-item invalid without affecting the others.
func TestInjectContextInvalidItems(t *testing.T) {
	items := ctxItems(t, `["bare-string", {"content":""}, {"content":42}, {"title":"no content"}, {"content":"good"}]`)
	out, rows := injectContext(`{}`, items, 2000, 8)
	got := statuses(rows)
	want := []string{ctxInjected /*guard*/, ctxInvalid, ctxInvalid, ctxInvalid, ctxInvalid, ctxInjected}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
	}
	if texts := systemTexts(t, out); len(texts) != 2 {
		t.Errorf("blocks = %d, want guard + 1", len(texts))
	}
}

// TestInjectContextLabelSanitization: control chars/newlines stripped,
// 200-byte cap — labels can't break the header framing.
func TestInjectContextLabelSanitization(t *testing.T) {
	long := strings.Repeat("s", 300)
	items := ctxItems(t, `[{"source":"a\nbc","title":"`+long+`","content":"x"}]`)
	out, rows := injectContext(`{}`, items, 2000, 8)
	if rows[1].Source != "abc" {
		t.Errorf("sanitized source = %q", rows[1].Source)
	}
	if len(rows[1].Title) != 200 {
		t.Errorf("title cap = %d bytes", len(rows[1].Title))
	}
	block := systemTexts(t, out)[1]
	if !strings.Contains(block, "Source: abc\n") {
		t.Errorf("header line mangled:\n%s", block)
	}
}

// TestInjectContextEmpty: an empty array injects nothing and returns no
// rows at all.
func TestInjectContextEmpty(t *testing.T) {
	req := `{"system":"s"}`
	out, rows := injectContext(req, ctxItems(t, `[]`), 2000, 8)
	if out != req || len(rows) != 0 {
		t.Errorf("empty items: out mutated or rows = %+v", rows)
	}
}
