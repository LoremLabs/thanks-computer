package processor

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
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
// records the Meta it was called with, so a test can see what WITH
// resolved to on each pass.
type pager struct {
	mu    sync.Mutex
	hits  int
	metas []string

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

func newRepeatUnit(t *testing.T) *Unit {
	t.Helper()
	pu, _ := newTestUnit(t)
	if pu.Conf.OpTimeout == "" {
		pu.Conf.OpTimeout = "5s"
	}
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
func runStage(t *testing.T, ctx context.Context, pu *Unit, stage string) string {
	t.Helper()
	resCh := make(chan event.Payload, 4)
	done := make(chan error, 1)
	go func() { done <- pu.Run(ctx, `{}`, stage, resCh) }()
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

const pagerLoop = `WITH after = ._p.next, repeat_until = ._p.next == "", repeat_max = 10 EXEC "txco://pager"`

func rowsOf(raw string) []int64 {
	var rows []int64
	for _, r := range gjson.Get(raw, "_p.rows").Array() {
		rows = append(rows, r.Int())
	}
	return rows
}

// TestRepeatCursorDrain is the headline shape: WITH re-resolves each
// pass against the accumulated view (the handler sees the previous
// page's cursor), the loop stops when the predicate holds, every page's
// rows accumulate, and the bookkeeping lands under _txc.runtime.repeat.
func TestRepeatCursorDrain(t *testing.T) {
	pu := newRepeatUnit(t)
	p := &pager{pages: 3}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", pagerLoop)

	out := runStage(t, context.Background(), pu, "loop/0")

	if got := p.hitCount(); got != 3 {
		t.Fatalf("hits = %d, want 3 (out=%s)", got, out)
	}
	if got := rowsOf(out); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("rows = %v, want [1 2 3] — passes must append, not overwrite", got)
	}
	if got := gjson.Get(out, "_p.next").String(); got != "" {
		t.Errorf("_p.next = %q, want \"\" (last page's cursor wins)", got)
	}
	book := gjson.Get(out, "_txc.runtime.repeat.pager")
	if book.Get("passes").Int() != 3 || book.Get("stop").String() != "done" {
		t.Errorf("bookkeeping = %s, want passes 3 / stop done", book.Raw)
	}
	if book.Get("error").Exists() {
		t.Errorf("bookkeeping carries an error on a clean drain: %s", book.Raw)
	}

	// Pass 1 has no cursor yet (missing path → null); passes 2 and 3
	// carry the previous page's `next`. repeat_max never reaches the op.
	if got := gjson.Get(p.meta(0), "after"); got.Type != gjson.Null {
		t.Errorf("pass 1 after = %s, want null", got.Raw)
	}
	if got := gjson.Get(p.meta(1), "after").String(); got != "2" {
		t.Errorf("pass 2 after = %q, want \"2\" — WITH is not re-resolved against the view", got)
	}
	if got := gjson.Get(p.meta(2), "after").String(); got != "3" {
		t.Errorf("pass 3 after = %q, want \"3\"", got)
	}
	for i := 0; i < 3; i++ {
		if gjson.Get(p.meta(i), "repeat_max").Exists() {
			t.Errorf("pass %d Meta leaks repeat_max: %s", i+1, p.meta(i))
		}
	}
}

// TestRepeatMaxTruncates: an endless cursor stops at repeat_max with
// everything so far kept and the run still succeeding.
func TestRepeatMaxTruncates(t *testing.T) {
	pu := newRepeatUnit(t)
	p := &pager{} // endless
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH after = ._p.next, repeat_until = ._p.next == "", repeat_max = 3 EXEC "txco://pager"`)

	out := runStage(t, context.Background(), pu, "loop/0")

	if got := p.hitCount(); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
	if got := rowsOf(out); len(got) != 3 {
		t.Errorf("rows = %v, want three pages kept", got)
	}
	book := gjson.Get(out, "_txc.runtime.repeat.pager")
	if book.Get("passes").Int() != 3 || book.Get("stop").String() != "max" {
		t.Errorf("bookkeeping = %s, want passes 3 / stop max", book.Raw)
	}
}

// TestRepeatStopsOnFuel is the change that makes in-scope fuel real for
// a loop: scope-enter 10 + 25 per pass crosses a 100 ceiling on the
// fourth pass, and the loop notices between passes instead of running
// to repeat_max. Truncate + flag: the run itself still completes.
func TestRepeatStopsOnFuel(t *testing.T) {
	pu := withBudget(t, 100, 0, 0)
	if pu.Conf.OpTimeout == "" {
		pu.Conf.OpTimeout = "5s"
	}
	p := &pager{}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH after = ._p.next, repeat_until = ._p.next == "", repeat_max = 50 EXEC "txco://pager"`)

	out := runStage(t, context.Background(), pu, "loop/0")

	if got := p.hitCount(); got != 4 {
		t.Fatalf("hits = %d, want 4 (10 + 4×25 = 110 > 100)", got)
	}
	book := gjson.Get(out, "_txc.runtime.repeat.pager")
	if book.Get("passes").Int() != 4 || book.Get("stop").String() != "fuel" {
		t.Errorf("bookkeeping = %s, want passes 4 / stop fuel", book.Raw)
	}
	if got := rowsOf(out); len(got) != 4 {
		t.Errorf("rows = %v, want the four completed pages kept", got)
	}
}

// TestRepeatErrorTruncates: a failing pass ends the loop with the
// earlier passes' output kept and the error recorded — never a lost
// contribution, never a failed run.
func TestRepeatErrorTruncates(t *testing.T) {
	pu := newRepeatUnit(t)
	p := &pager{failOn: 3}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", pagerLoop)

	out := runStage(t, context.Background(), pu, "loop/0")

	if got := p.hitCount(); got != 3 {
		t.Fatalf("hits = %d, want 3", got)
	}
	if got := rowsOf(out); len(got) != 2 {
		t.Errorf("rows = %v, want the two good pages kept", got)
	}
	book := gjson.Get(out, "_txc.runtime.repeat.pager")
	if book.Get("passes").Int() != 3 || book.Get("stop").String() != "error" {
		t.Errorf("bookkeeping = %s, want passes 3 / stop error", book.Raw)
	}
	if !strings.Contains(book.Get("error").String(), "page 3 unavailable") {
		t.Errorf("bookkeeping error = %q, want the handler's message", book.Get("error").String())
	}
}

// TestRepeatTimeoutKeepsPartial: WITH timeout bounds the whole loop.
// Before the loop existed a deadline lost the op's output entirely;
// now the passes that finished are kept and the stop says why.
func TestRepeatTimeoutKeepsPartial(t *testing.T) {
	pu := newRepeatUnit(t)
	p := &pager{delay: 40 * time.Millisecond}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager",
		`WITH timeout = 150, after = ._p.next, repeat_until = ._p.next == "", repeat_max = 50 EXEC "txco://pager"`)

	out := runStage(t, context.Background(), pu, "loop/0")

	book := gjson.Get(out, "_txc.runtime.repeat.pager")
	if book.Get("stop").String() != "timeout" {
		t.Fatalf("bookkeeping = %s, want stop timeout", book.Raw)
	}
	rows := rowsOf(out)
	if len(rows) < 1 || len(rows) >= 50 {
		t.Errorf("rows = %v, want the completed passes kept (some, fewer than repeat_max)", rows)
	}
	if got := book.Get("passes").Int(); got != int64(len(rows))+1 {
		t.Errorf("passes = %d, want completed rows (%d) + the pass the deadline cut", got, len(rows))
	}
}

// TestRepeatAdmission: a loop the chassis cannot honor is dropped at
// dispatch — no pass runs, the sibling op in the same scope is
// unaffected, and no bookkeeping is written.
func TestRepeatAdmission(t *testing.T) {
	cases := []struct {
		name string
		txcl string
		ceil int
	}{
		{"missing repeat_max", `WITH after = ._p.next, repeat_until = ._p.next == "" EXEC "txco://pager"`, 0},
		{"non-positive repeat_max", `WITH repeat_until = ._p.next == "", repeat_max = 0 EXEC "txco://pager"`, 0},
		{"repeat_max over the ceiling", `WITH repeat_until = ._p.next == "", repeat_max = 6 EXEC "txco://pager"`, 5},
		{"non-txco EXEC", `WITH repeat_until = ._p.next == "", repeat_max = 2 EXEC "https://127.0.0.1:1/never"`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pu := newRepeatUnit(t)
			pu.Conf.OpRepeatMax = tc.ceil
			p := &pager{}
			pu.Handle([]byte("txco://pager"), p.handler())
			seedNamedOp(t, pu, "loop", 0, "pager", tc.txcl)
			seedNamedOp(t, pu, "loop", 0, "sibling", `EMIT .sibling = true`)

			out := runStage(t, context.Background(), pu, "loop/0")

			if got := p.hitCount(); got != 0 {
				t.Errorf("hits = %d, want 0 — a rejected loop must not dispatch", got)
			}
			if !gjson.Get(out, "sibling").Bool() {
				t.Errorf("sibling op lost: %s", out)
			}
			if gjson.Get(out, "_txc.runtime.repeat").Exists() {
				t.Errorf("bookkeeping written for a rejected loop: %s", out)
			}
		})
	}
}

// TestRepeatMaxStrippedFromOrdinaryOp: repeat_* keys are chassis
// directives, so a stray repeat_max on a non-looping op is stripped
// before the handler sees Meta and the op runs exactly once.
func TestRepeatMaxStrippedFromOrdinaryOp(t *testing.T) {
	pu := newRepeatUnit(t)
	p := &pager{}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", `WITH repeat_max = 5, tag = "x" EXEC "txco://pager"`)

	out := runStage(t, context.Background(), pu, "loop/0")

	if got := p.hitCount(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if m := p.meta(0); gjson.Get(m, "repeat_max").Exists() || gjson.Get(m, "tag").String() != "x" {
		t.Errorf("Meta = %s, want tag kept and repeat_max stripped", m)
	}
	if gjson.Get(out, "_txc.runtime.repeat").Exists() {
		t.Errorf("bookkeeping written for a single-shot op: %s", out)
	}
}

// TestRepeatTwoLoopsOneScope: bookkeeping is keyed by op name so two
// loops in one scope report independently (scalars are last-write-wins
// in the scope merge; a shared key would clobber).
func TestRepeatTwoLoopsOneScope(t *testing.T) {
	pu := newRepeatUnit(t)
	pa, pb := &pager{pages: 2}, &pager{pages: 3}
	pu.Handle([]byte("txco://pa"), pa.handler())
	pu.Handle([]byte("txco://pb"), pb.handler())
	seedNamedOp(t, pu, "loop", 0, "a",
		`WITH after = ._p.next, repeat_until = ._p.next == "", repeat_max = 10 EXEC "txco://pa"`)
	seedNamedOp(t, pu, "loop", 0, "b",
		`WITH after = ._p.next, repeat_until = ._p.next == "", repeat_max = 10 EXEC "txco://pb"`)

	out := runStage(t, context.Background(), pu, "loop/0")

	if got := gjson.Get(out, "_txc.runtime.repeat.a.passes").Int(); got != 2 {
		t.Errorf("a.passes = %d, want 2 (%s)", got, out)
	}
	if got := gjson.Get(out, "_txc.runtime.repeat.b.passes").Int(); got != 3 {
		t.Errorf("b.passes = %d, want 3 (%s)", got, out)
	}
}

// TestRepeatTraceShape: one step for the whole loop, carrying passes and
// stop_reason in meta.json with the accumulated output, plus one op.pass
// timeline line per pass (file sink only).
func TestRepeatTraceShape(t *testing.T) {
	dir := t.TempDir()
	sink, err := trace.Open("file", trace.StoreConfig{Dir: dir, Mode: trace.ModeFull})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	tr := sink.Begin(trace.RequestInfo{
		RID: "CbRepeatTrace001", Src: "test", Tenant: "t", Stack: "loop",
		StartedAt: time.Now(), Payload: []byte(`{}`),
	})
	ctx := trace.WithContext(context.Background(), tr)

	pu := newRepeatUnit(t)
	p := &pager{pages: 3}
	pu.Handle([]byte("txco://pager"), p.handler())
	seedNamedOp(t, pu, "loop", 0, "pager", pagerLoop)

	out := runStage(t, ctx, pu, "loop/0")
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
	// Ordinary steps carry neither key.
	for _, m := range metas {
		if m["name"] == "pager" {
			continue
		}
		if _, ok := m["passes"]; ok {
			t.Errorf("single-shot step %v carries passes", m["name"])
		}
	}
}
