package processor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/tidwall/gjson"
)

// pager is a stateful txco:// handler standing in for a cursor-paged
// store: hit n answers {"_p":{"next":"<n+1>" | "","rows":[n]}} and
// records the Meta and input it was called with, so a test can see what
// WITH and LOOP SET resolved to on each pass.
type pager struct {
	mu     sync.Mutex
	hits   int
	metas  []string
	inputs []string

	pages  int           // pages available; the last one answers next "". 0 = endless
	failOn int           // hit number that returns an error; 0 = never
	delay  time.Duration // per-hit sleep, honoring ctx
}

func (p *pager) handler() event.OpsHandler {
	return event.OpsHandlerFunc(func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
		p.mu.Lock()
		p.hits++
		n := p.hits
		p.metas = append(p.metas, operation.MetaFromContext(ctx))
		p.inputs = append(p.inputs, string(in))
		p.mu.Unlock()
		if p.delay > 0 {
			select {
			case <-time.After(p.delay):
			case <-ctx.Done():
				return event.Payload{}, ctx.Err()
			}
		}
		if p.failOn > 0 && n == p.failOn {
			return event.Payload{}, errors.New("page " + strconv.Itoa(n) + " unavailable")
		}
		next := strconv.Itoa(n + 1)
		if p.pages > 0 && n >= p.pages {
			next = ""
		}
		return event.Payload{
			Raw:  `{"_p":{"next":"` + next + `","rows":[` + strconv.Itoa(n) + `]}}`,
			Type: event.JSON,
		}, nil
	})
}

func (p *pager) hitCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

func (p *pager) meta(i int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.metas) {
		return ""
	}
	return p.metas[i]
}

func (p *pager) input(i int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.inputs) {
		return ""
	}
	return p.inputs[i]
}

func newLoopUnit(t *testing.T) *Unit {
	t.Helper()
	pu, _ := newTestUnit(t)
	if pu.Conf.OpTimeout == "" {
		pu.Conf.OpTimeout = "5s"
	}
	pu.Conf.LoopTimeout = "60s"
	return pu
}

func seedNamedOp(t *testing.T, pu *Unit, stack string, scope int, name, txcl string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		stack, scope, name, txcl,
	); err != nil {
		t.Fatalf("seed %s/%d/%s: %v", stack, scope, name, err)
	}
}

// runStage drives pu.Run to completion and returns the final payload.
// The loop must finish inside the run — a hung loop shows up here as a
// timeout rather than a hung test binary.
func runStage(t *testing.T, ctx context.Context, pu *Unit, raw, stage string) string {
	t.Helper()
	resCh := make(chan event.Payload, 4)
	done := make(chan error, 1)
	go func() { done <- pu.Run(ctx, raw, stage, resCh) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s")
	}
	select {
	case p := <-resCh:
		return p.Raw
	default:
		t.Fatal("Run emitted no payload")
	}
	return ""
}

// pagerDrain: the everyday cursor drain — WITH after re-resolves each
// pass against the view; no pause between pages.
const pagerDrain = `WITH after = ._p.next EXEC "txco://pager" LOOP EVERY "2ms" UNTIL ._p.next == "" MAX 10`

func rowsOf(raw string) []int64 {
	var rows []int64
	for _, r := range gjson.Get(raw, "_p.rows").Array() {
		rows = append(rows, r.Int())
	}
	return rows
}

func book(raw, name string) gjson.Result {
	return gjson.Get(raw, "_txc.runtime.loop."+name)
}

// TestLoopCursorDrain is the headline shape: WITH re-resolves each pass
// against the accumulated view (the handler sees the previous page's
// cursor), the loop stops when UNTIL holds, every page's rows
// accumulate, and the bookkeeping lands under _txc.runtime.loop.
func TestLoopCursorDrain(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{pages: 3}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", pagerDrain)

	out := runStage(t, context.Background(), pu, `{}`, "loop/0")

	if got := p.hitCount(); got != 3 {
		t.Fatalf("hits = %d, want 3 (out=%s)", got, out)
	}
	if got := rowsOf(out); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("rows = %v, want [1 2 3] — passes must append, not overwrite", got)
	}
	if got := gjson.Get(out, "_p.next").String(); got != "" {
		t.Errorf("_p.next = %q, want \"\" (last page's cursor wins)", got)
	}
	b := book(out, "pager")
	if b.Get("passes").Int() != 3 || b.Get("stop").String() != "done" {
		t.Errorf("bookkeeping = %s, want passes 3 / stop done", b.Raw)
	}
	if b.Get("error").Exists() {
		t.Errorf("bookkeeping carries an error on a clean drain: %s", b.Raw)
	}
	// Pass 1 has no cursor yet (missing path → null); passes 2 and 3
	// carry the previous page's `next`.
	if got := gjson.Get(p.meta(0), "after"); got.Type != gjson.Null {
		t.Errorf("pass 1 after = %s, want null", got.Raw)
	}
	if got := gjson.Get(p.meta(1), "after").String(); got != "2" {
		t.Errorf("pass 2 after = %q, want \"2\" — WITH is not re-resolved against the view", got)
	}
	if got := gjson.Get(p.meta(2), "after").String(); got != "3" {
		t.Errorf("pass 3 after = %q, want \"3\"", got)
	}
}

// TestLoopMaxTruncates: an endless cursor stops at MAX with everything
// so far kept and the run still succeeding.
func TestLoopMaxTruncates(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{} // endless
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH after = ._p.next EXEC "txco://pager" LOOP EVERY "2ms" UNTIL ._p.next == "" MAX 3`)

	out := runStage(t, context.Background(), pu, `{}`, "loop/0")

	if got := p.hitCount(); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
	if got := rowsOf(out); len(got) != 3 {
		t.Errorf("rows = %v, want three pages kept", got)
	}
	if b := book(out, "pager"); b.Get("passes").Int() != 3 || b.Get("stop").String() != "max" {
		t.Errorf("bookkeeping = %s, want passes 3 / stop max", b.Raw)
	}
}

// TestLoopDefaults: LOOP UNTIL alone runs at most 10 passes, paced at
// the default pause — the two-line poll behaves like a poll.
func TestLoopDefaults(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{} // endless
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", `EXEC "txco://pager" LOOP UNTIL ._p.next == ""`)

	start := time.Now()
	out := runStage(t, context.Background(), pu, `{}`, "loop/0")
	elapsed := time.Since(start)

	if got := p.hitCount(); got != 10 {
		t.Fatalf("hits = %d, want the default MAX of 10", got)
	}
	if b := book(out, "pager"); b.Get("stop").String() != "max" {
		t.Errorf("bookkeeping = %s, want stop max", b.Raw)
	}
	// Nine pauses at the 50ms default: the last pass never pays one.
	if elapsed < 9*loopDefaultEvery {
		t.Errorf("elapsed %v; ten passes at the default EVERY should take at least %v", elapsed, 9*loopDefaultEvery)
	}
}

// TestLoopEveryPaces: EVERY sets the pause between passes, the pause is
// not paid after the last pass, and the 2ms floor absorbs anything lower.
func TestLoopEveryPaces(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{pages: 4}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH after = ._p.next EXEC "txco://pager" LOOP EVERY "30ms" UNTIL ._p.next == "" MAX 10`)

	start := time.Now()
	runStage(t, context.Background(), pu, `{}`, "loop/0")
	elapsed := time.Since(start)
	if elapsed < 90*time.Millisecond {
		t.Errorf("elapsed %v; four passes at EVERY 30ms should take at least 90ms", elapsed)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("elapsed %v; EVERY 30ms should not cost more than three pauses", elapsed)
	}

	// EVERY 0 is floored, not treated as no pause and not an error.
	pu2 := newLoopUnit(t)
	p2 := &pager{pages: 3}
	pu2.Handle([]byte("txco://pager"), p2.handler())
	seedNamedOp(t, pu2, "loop", 0, "pager",
		`WITH after = ._p.next EXEC "txco://pager" LOOP EVERY 0 UNTIL ._p.next == "" MAX 10`)
	start = time.Now()
	out := runStage(t, context.Background(), pu2, `{}`, "loop/0")
	if b := book(out, "pager"); b.Get("passes").Int() != 3 {
		t.Errorf("bookkeeping = %s, want 3 passes with EVERY 0", b.Raw)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("elapsed %v; EVERY 0 should floor to 2ms, not the 50ms default", elapsed)
	}
}

// TestLoopStopsOnFuel is what makes in-scope fuel real for a loop:
// scope-enter 10 + 25 per pass crosses a 100 ceiling on the fourth pass,
// and the loop notices between passes instead of running to MAX.
func TestLoopStopsOnFuel(t *testing.T) {
	pu := withBudget(t, 100, 0, 0)
	if pu.Conf.OpTimeout == "" {
		pu.Conf.OpTimeout = "5s"
	}
	pu.Conf.LoopTimeout = "60s"
	p := &pager{}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH after = ._p.next EXEC "txco://pager" LOOP EVERY "2ms" UNTIL ._p.next == "" MAX 50`)

	out := runStage(t, context.Background(), pu, `{}`, "loop/0")

	if got := p.hitCount(); got != 4 {
		t.Fatalf("hits = %d, want 4 (10 + 4×25 = 110 > 100)", got)
	}
	if b := book(out, "pager"); b.Get("passes").Int() != 4 || b.Get("stop").String() != "fuel" {
		t.Errorf("bookkeeping = %s, want passes 4 / stop fuel", b.Raw)
	}
	if got := rowsOf(out); len(got) != 4 {
		t.Errorf("rows = %v, want the four completed pages kept", got)
	}
}

// TestLoopErrorTruncates: a failing pass ends the loop with the earlier
// passes' output kept and the error recorded — never a lost
// contribution, never a failed run.
func TestLoopErrorTruncates(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{failOn: 3}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", pagerDrain)

	out := runStage(t, context.Background(), pu, `{}`, "loop/0")

	if got := p.hitCount(); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
	if got := rowsOf(out); len(got) != 2 {
		t.Errorf("rows = %v, want the two good pages kept", got)
	}
	b := book(out, "pager")
	if b.Get("passes").Int() != 3 || b.Get("stop").String() != "error" {
		t.Errorf("bookkeeping = %s, want passes 3 / stop error", b.Raw)
	}
	if !strings.Contains(b.Get("error").String(), "page 3 unavailable") {
		t.Errorf("bookkeeping error = %q, want the handler's message", b.Get("error").String())
	}
}

// TestLoopTimeoutKeepsPartial: WITH timeout bounds the whole loop and
// wins over the loop default. The passes that finished are kept.
func TestLoopTimeoutKeepsPartial(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{delay: 40 * time.Millisecond}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH timeout = 150, after = ._p.next EXEC "txco://pager" LOOP EVERY "2ms" UNTIL ._p.next == "" MAX 50`)

	out := runStage(t, context.Background(), pu, `{}`, "loop/0")

	b := book(out, "pager")
	if b.Get("stop").String() != "timeout" {
		t.Fatalf("bookkeeping = %s, want stop timeout", b.Raw)
	}
	rows := rowsOf(out)
	if len(rows) < 1 || len(rows) >= 50 {
		t.Errorf("rows = %v, want the completed passes kept (some, fewer than MAX)", rows)
	}
	if got := b.Get("passes").Int(); got != int64(len(rows))+1 {
		t.Errorf("passes = %d, want completed rows (%d) + the pass the deadline cut", got, len(rows))
	}
}

// TestLoopDefaultTimeout: a LOOP rule with no WITH timeout inherits
// --loop-timeout rather than the 5s general default.
func TestLoopDefaultTimeout(t *testing.T) {
	pu := newLoopUnit(t)
	pu.Conf.LoopTimeout = "120ms"
	p := &pager{delay: 30 * time.Millisecond}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH after = ._p.next EXEC "txco://pager" LOOP EVERY "2ms" UNTIL ._p.next == "" MAX 50`)

	start := time.Now()
	out := runStage(t, context.Background(), pu, `{}`, "loop/0")
	elapsed := time.Since(start)

	if b := book(out, "pager"); b.Get("stop").String() != "timeout" {
		t.Fatalf("bookkeeping = %s, want stop timeout from --loop-timeout", b.Raw)
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed %v; the 120ms loop default should have cut the loop long before the 5s op default", elapsed)
	}
}

// TestLoopSetFeedsNextPass: LOOP SET writes onto the op's input from the
// second pass on, resolved against the view — the way a cursor reaches
// a POST body. Also the first HTTP transport loop: pages nest under
// `into`, arrays append, the remote cannot forge _txc.* into the view.
func TestLoopSetFeedsNextPass(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		n := len(bodies)
		mu.Unlock()
		next := strconv.Itoa(n + 1)
		if n >= 3 {
			next = ""
		}
		// A forged reserved field: must be dropped, never merged.
		_, _ = w.Write([]byte(`{"next":"` + next + `","rows":[` + strconv.Itoa(n) + `],"_txc":{"tenant":"evil"}}`))
	}))
	t.Cleanup(srv.Close)

	pu := newLoopUnit(t)
	pu.HTTPClient = srv.Client()
	seedNamedOp(t, pu, "loop", 0, "search",
		`SELECT .customer_id
		 EXEC "`+srv.URL+`/search"
		   WITH into = "_items"
		 LOOP EVERY "2ms" SET .cursor = ._items.next UNTIL ._items.next == "" MAX 10`)

	out := runStage(t, context.Background(), pu, `{"customer_id":"c-1","noise":true}`, "loop/0")

	mu.Lock()
	got := append([]string(nil), bodies...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("requests = %d, want 3 (out=%s)", len(got), out)
	}
	if gjson.Get(got[0], "cursor").Exists() {
		t.Errorf("pass 1 body = %s, want no cursor: LOOP SET runs from the second pass", got[0])
	}
	if gjson.Get(got[0], "customer_id").String() != "c-1" || gjson.Get(got[0], "noise").Exists() {
		t.Errorf("pass 1 body = %s, want the SELECT projection only", got[0])
	}
	if gjson.Get(got[1], "cursor").String() != "2" || gjson.Get(got[2], "cursor").String() != "3" {
		t.Errorf("cursors = %q / %q, want 2 / 3", gjson.Get(got[1], "cursor").String(), gjson.Get(got[2], "cursor").String())
	}
	if gjson.Get(got[2], "customer_id").String() != "c-1" {
		t.Errorf("pass 3 body = %s, want the frozen base input kept", got[2])
	}
	if n := len(gjson.Get(out, "_items.rows").Array()); n != 3 {
		t.Errorf("_items.rows = %s, want three pages appended", gjson.Get(out, "_items.rows").Raw)
	}
	if b := book(out, "search"); b.Get("stop").String() != "done" || b.Get("passes").Int() != 3 {
		t.Errorf("bookkeeping = %s, want passes 3 / stop done", b.Raw)
	}
	if gjson.Get(out, "_txc.tenant").String() == "evil" {
		t.Errorf("remote forged _txc.tenant into the envelope: %s", out)
	}
	if gjson.Get(out, "cursor").Exists() {
		t.Errorf("LOOP SET leaked into the op's output: %s", out)
	}
}

// TestLoopSetCounter: a LOOP SET can read its own previous value from
// the view, so a page number counts up across passes.
func TestLoopSetCounter(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{pages: 3}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`SET .page = 1 EXEC "txco://pager" LOOP EVERY "2ms" SET .page = &add(.page, 1) UNTIL ._p.next == "" MAX 5`)

	runStage(t, context.Background(), pu, `{}`, "loop/0")

	for i, want := range []int64{1, 2, 3} {
		if got := gjson.Get(p.input(i), "page").Int(); got != want {
			t.Errorf("pass %d input page = %d, want %d (input=%s)", i+1, got, want, p.input(i))
		}
	}
}

// TestLoopURLCursor: the other HTTP cursor style — WITH url re-resolves
// every pass, so the cursor rides the query string with no SET.
func TestLoopURLCursor(t *testing.T) {
	var mu sync.Mutex
	var afters []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		afters = append(afters, r.URL.Query().Get("after"))
		n := len(afters)
		mu.Unlock()
		next := strconv.Itoa(n + 1)
		if n >= 3 {
			next = ""
		}
		_, _ = w.Write([]byte(`{"next":"` + next + `","rows":[` + strconv.Itoa(n) + `]}`))
	}))
	t.Cleanup(srv.Close)

	pu := newLoopUnit(t)
	pu.HTTPClient = srv.Client()
	seedNamedOp(t, pu, "loop", 0, "items",
		`EXEC "`+srv.URL+`/items"
		   WITH method = "GET",
		        url = &concat("`+srv.URL+`/items?after=", ._items.next),
		        into = "_items"
		 LOOP EVERY "2ms" UNTIL ._items.next == "" MAX 10`)

	out := runStage(t, context.Background(), pu, `{}`, "loop/0")

	mu.Lock()
	got := append([]string(nil), afters...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "" || got[1] != "2" || got[2] != "3" {
		t.Errorf("after params = %q, want [\"\" \"2\" \"3\"]", got)
	}
	if b := book(out, "items"); b.Get("stop").String() != "done" {
		t.Errorf("bookkeeping = %s, want stop done", b.Raw)
	}
}

// TestLoopHaltedBySibling: a sibling op at the same stage emitting
// @halt cuts the loop at once — mid-pass or mid-pause — with the
// partial output kept and stop "halted".
func TestLoopHaltedBySibling(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{delay: 20 * time.Millisecond} // endless, slow
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH after = ._p.next EXEC "txco://pager" LOOP EVERY "2ms" UNTIL ._p.next == "" MAX 200`)
	seedNamedOp(t, pu, "loop", 0, "gate", `EMIT .denied = true, @halt = true`)

	start := time.Now()
	out := runStage(t, context.Background(), pu, `{}`, "loop/0")
	elapsed := time.Since(start)

	b := book(out, "pager")
	if b.Get("stop").String() != "halted" {
		t.Fatalf("bookkeeping = %s, want stop halted", b.Raw)
	}
	if got := b.Get("passes").Int(); got >= 200 || got < 1 {
		t.Errorf("passes = %d, want the loop cut early", got)
	}
	if elapsed > 2*time.Second {
		t.Errorf("elapsed %v; the halt should have cut the loop immediately, not after 200 passes", elapsed)
	}
	if !gjson.Get(out, "denied").Bool() {
		t.Errorf("sibling's output missing: %s", out)
	}
}

// TestLoopGotoDoesNotPreempt: a sibling's @goto is a later part of the
// stage lifecycle — the loop runs to its own end and its output rides
// the envelope into the target stage.
func TestLoopGotoDoesNotPreempt(t *testing.T) {
	pu := newLoopUnit(t)
	p := &pager{pages: 3, delay: 10 * time.Millisecond}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", pagerDrain)
	seedNamedOp(t, pu, "loop", 0, "jump", `EMIT @goto = "loop/100"`)
	seedNamedOp(t, pu, "loop", 100, "after", `WHEN @runtime.loop.pager.stop == "done" EMIT .landed = true`)

	out := runStage(t, context.Background(), pu, `{}`, "loop/0")

	if got := p.hitCount(); got != 3 {
		t.Errorf("hits = %d, want 3: a goto must not cut the loop", got)
	}
	if !gjson.Get(out, "landed").Bool() {
		t.Errorf("next stage did not see the loop's bookkeeping: %s", out)
	}
}

// TestLoopAdmission: a loop the chassis cannot honor is dropped at
// dispatch — no pass runs, the sibling op in the same scope is
// unaffected, and no bookkeeping is written.
func TestLoopAdmission(t *testing.T) {
	cases := []struct {
		name string
		txcl string
		raw  string
		ceil int
	}{
		{"MAX over the ceiling", `EXEC "txco://pager" LOOP UNTIL ._p.next == "" MAX 6`, `{}`, 5},
		{"compute is held back", `EXEC "compute://sha256/abc" LOOP UNTIL ._p.next == "" MAX 2`, `{}`, 0},
		{"ai is held back", `EXEC "ai://chat" LOOP UNTIL ._p.next == "" MAX 2`, `{}`, 0},
		{"path-valued async mode", `WITH mode = ._m EXEC "https://127.0.0.1:1/never" LOOP UNTIL .status == "done"`, `{"_m":"async"}`, 0},
		{"path-valued continuable mode", `WITH mode = ._m EXEC "txco://pager" LOOP UNTIL ._p.next == ""`, `{"_m":"continuable"}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pu := newLoopUnit(t)
			pu.Conf.OpLoopMax = tc.ceil
			p := &pager{}
			pu.Handle([]byte("txco://pager"), p.handler())
			seedNamedOp(t, pu, "loop", 0, "pager", tc.txcl)
			seedNamedOp(t, pu, "loop", 0, "sibling", `EMIT .sibling = true`)

			out := runStage(t, context.Background(), pu, tc.raw, "loop/0")

			if got := p.hitCount(); got != 0 {
				t.Errorf("hits = %d, want 0 — a rejected loop must not dispatch", got)
			}
			if !gjson.Get(out, "sibling").Bool() {
				t.Errorf("sibling op lost: %s", out)
			}
			if gjson.Get(out, "_txc.runtime.loop").Exists() {
				t.Errorf("bookkeeping written for a rejected loop: %s", out)
			}
		})
	}
}

// TestLoopTwoLoopsOneScope: bookkeeping is keyed by op name so two loops
// in one scope report independently.
func TestLoopTwoLoopsOneScope(t *testing.T) {
	pu := newLoopUnit(t)
	pa, pb := &pager{pages: 2}, &pager{pages: 3}
	pu.Handle([]byte("txco://pa"), pa.handler())
	pu.Handle([]byte("txco://pb"), pb.handler())
	seedNamedOp(t, pu, "loop", 0, "a", `WITH after = ._p.next EXEC "txco://pa" LOOP EVERY "2ms" UNTIL ._p.next == ""`)
	seedNamedOp(t, pu, "loop", 0, "b", `WITH after = ._p.next EXEC "txco://pb" LOOP EVERY "2ms" UNTIL ._p.next == ""`)

	out := runStage(t, context.Background(), pu, `{}`, "loop/0")

	if got := book(out, "a").Get("passes").Int(); got != 2 {
		t.Errorf("a.passes = %d, want 2 (%s)", got, out)
	}
	if got := book(out, "b").Get("passes").Int(); got != 3 {
		t.Errorf("b.passes = %d, want 3 (%s)", got, out)
	}
}

// TestLoopTraceShape: one step for the whole loop, carrying passes and
// stop_reason in meta.json with the accumulated output, plus one op.pass
// timeline line per pass (file sink only).
func TestLoopTraceShape(t *testing.T) {
	dir := t.TempDir()
	sink, err := trace.Open("file", trace.StoreConfig{Dir: dir, Mode: trace.ModeFull})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	tr := sink.Begin(trace.RequestInfo{
		RID: "CbLoopTrace0001", Src: "test", Tenant: "t", Stack: "loop",
		StartedAt: time.Now(), Payload: []byte(`{}`),
	})
	ctx := trace.WithContext(context.Background(), tr)

	pu := newLoopUnit(t)
	p := &pager{pages: 3}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", pagerDrain)

	out := runStage(t, ctx, pu, `{}`, "loop/0")
	tr.End("ok", "", []byte(out))
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("close sink: %v", err)
	}

	var metas []map[string]any
	var timeline string
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch d.Name() {
		case "meta.json":
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			var m map[string]any
			if jerr := json.Unmarshal(b, &m); jerr != nil {
				return jerr
			}
			metas = append(metas, m)
		case "timeline.jsonl":
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			timeline = string(b)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk trace dir: %v", walkErr)
	}

	var loopSteps []map[string]any
	for _, m := range metas {
		if m["name"] == "pager" {
			loopSteps = append(loopSteps, m)
		}
	}
	if len(loopSteps) != 1 {
		t.Fatalf("pager steps = %d, want exactly one for the whole loop (metas=%v)", len(loopSteps), metas)
	}
	st := loopSteps[0]
	if st["passes"] != float64(3) || st["stop_reason"] != "done" || st["status"] != "ok" {
		t.Errorf("loop step meta = %v, want passes 3 / stop_reason done / status ok", st)
	}
	if got := strings.Count(timeline, `"event":"op.pass"`); got != 3 {
		t.Errorf("op.pass timeline lines = %d, want 3\n%s", got, timeline)
	}
	for _, m := range metas {
		if m["name"] == "pager" {
			continue
		}
		if _, ok := m["passes"]; ok {
			t.Errorf("single-shot step %v carries passes", m["name"])
		}
	}
}
