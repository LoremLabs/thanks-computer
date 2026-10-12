package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// The no-call oracle. A request that makes no call must run exactly as it
// did before the call engine existed: the same final envelope, the same
// trace events in the same order, the same steps. The goldens under
// testdata/call_oracle were recorded at 53965b6, before any frame code
// touched the processor; `TXCO_UPDATE_GOLDEN=1 go test -run TestNoCallOracle`
// re-records them, which is only legitimate for a deliberate behaviour
// change that a reviewer has signed off on.

// oracleTracer keeps what a request's tracer saw, in order, without the
// clocks: event names with their fields, and one line per step.
type oracleTracer struct {
	mu     sync.Mutex
	events []string
	steps  []string
}

func (r *oracleTracer) Step(info trace.StepInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, fmt.Sprintf("%s/%d/%s %s %s %s",
		info.Stack, info.Scope, info.Name, info.Transport, info.Status, info.Error))
}

func (r *oracleTracer) Event(ev trace.TimelineEvent) {
	keys := make([]string, 0, len(ev.Fields))
	for k := range ev.Fields {
		if k == "ms" {
			continue // the one wall-clock field a stage event carries
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(ev.Event)
	for _, k := range keys {
		fmt.Fprintf(&b, " %s=%v", k, ev.Fields[k])
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, b.String())
}

func (r *oracleTracer) End(status, reason string, final []byte) {}

// oracleFlow is one representative request: the rules it runs on, the
// envelope it enters with, and where.
type oracleFlow struct {
	name  string
	rules []struct {
		stack string
		scope int
		name  string
		txcl  string
	}
	in    string
	stage string
}

func rule(stack string, scope int, name, txcl string) struct {
	stack string
	scope int
	name  string
	txcl  string
} {
	return struct {
		stack string
		scope int
		name  string
		txcl  string
	}{stack, scope, name, txcl}
}

var oracleFlows = []oracleFlow{
	{"two-scopes", []struct {
		stack string
		scope int
		name  string
		txcl  string
	}{
		rule("flow", 100, "a", `EMIT .a = 1`),
		rule("flow", 200, "b", `EMIT .b = 2`),
	}, `{"_txc":{"stack":"flow","src":"http"},"q":"hi"}`, "flow/0"},

	{"goto-in-stack", []struct {
		stack string
		scope int
		name  string
		txcl  string
	}{
		rule("flow", 100, "jump", `EMIT @goto = "flow/300"`),
		rule("flow", 200, "skipped", `EMIT .skipped = true`),
		rule("flow", 300, "landed", `EMIT .landed = true`),
	}, `{"_txc":{"stack":"flow","src":"http"}}`, "flow/0"},

	{"unschemed-jump-across-stacks", []struct {
		stack string
		scope int
		name  string
		txcl  string
	}{
		rule("flow", 100, "handoff", `EXEC "other/100"`),
		rule("flow", 200, "never", `EMIT .never = true`),
		rule("other", 100, "o1", `EMIT .other = 1`),
		rule("other", 200, "o2", `EMIT .other2 = 2`),
	}, `{"_txc":{"stack":"flow","src":"http"}}`, "flow/0"},

	{"halt", []struct {
		stack string
		scope int
		name  string
		txcl  string
	}{
		rule("flow", 100, "stop", `EMIT .x = 1, @halt = true`),
		rule("flow", 200, "never", `EMIT .never = true`),
	}, `{"_txc":{"stack":"flow","src":"http"}}`, "flow/0"},

	{"overlay-peels-to-parent", []struct {
		stack string
		scope int
		name  string
		txcl  string
	}{
		rule("web", 100, "w100", `EMIT .web100 = true`),
		rule("web", 200, "w200", `EMIT .web200 = true`),
		rule("web/canary", 100, "c100", `EMIT .canary100 = true`),
	}, `{"_txc":{"stack":"web/canary","src":"http"}}`, "web/canary/0"},

	{"underscore-child-never-peels", []struct {
		stack string
		scope int
		name  string
		txcl  string
	}{
		rule("web", 100, "w100", `EMIT .web100 = true`),
		rule("web", 200, "w200", `EMIT .web200 = true`),
		rule("web/_mail", 100, "m100", `EMIT .mail100 = true`),
	}, `{"_txc":{"stack":"web/_mail","src":"lmtp"}}`, "web/_mail/0"},

	{"ping-pong-pays-the-repeat", []struct {
		stack string
		scope int
		name  string
		txcl  string
	}{
		rule("pp", 100, "to150", `WHEN .done != true EMIT @goto = "150"`),
		rule("pp", 150, "back", `WHEN .k2 != true EMIT .k2 = true, @goto = "100"`),
	}, `{"_txc":{"stack":"pp","src":"http"}}`, "pp/0"},

	{"exec-and-select", []struct {
		stack string
		scope int
		name  string
		txcl  string
	}{
		rule("flow", 100, "cap", `SELECT .q AS .q EXEC "txco://test-capture"`),
		rule("flow", 200, "after", `EMIT .after = true`),
	}, `{"_txc":{"stack":"flow","src":"http"},"q":"hi","noise":true}`, "flow/0"},
}

type oracleRecord struct {
	Final  string   `json:"final"`
	Events []string `json:"events"`
	Steps  []string `json:"steps"`
}

func TestNoCallOracle(t *testing.T) {
	update := os.Getenv("TXCO_UPDATE_GOLDEN") != ""
	dir := filepath.Join("testdata", "call_oracle")
	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range oracleFlows {
		t.Run(f.name, func(t *testing.T) {
			pu, _ := newTestUnit(t)
			captureHandler(t, pu, "txco://test-capture")
			for _, r := range f.rules {
				seedOp(t, pu, r.stack, r.scope, r.name, r.txcl)
			}
			rec := &oracleTracer{}
			ctx := trace.WithContext(context.Background(), rec)
			resCh := make(chan event.Payload, 4)
			if err := pu.Run(ctx, f.in, f.stage, resCh); err != nil {
				t.Fatalf("Run: %v", err)
			}
			var final string
			select {
			case p := <-resCh:
				final = p.Raw
			default:
				t.Fatal("no response")
			}
			// `_txc._seen` is written from a map, so its order is not a
			// fact of the flow; compare it sorted.
			if seen := gjson.Get(final, "_txc._seen"); seen.IsArray() {
				keys := make([]string, 0, len(seen.Array()))
				for _, k := range seen.Array() {
					keys = append(keys, k.String())
				}
				sort.Strings(keys)
				final, _ = sjson.Set(final, "_txc._seen", keys)
			}
			got := oracleRecord{Final: final, Events: rec.events, Steps: rec.steps}
			gotJSON, _ := json.MarshalIndent(got, "", "  ")
			path := filepath.Join(dir, f.name+".json")
			if update {
				if err := os.WriteFile(path, append(gotJSON, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("no golden for %s (record it with TXCO_UPDATE_GOLDEN=1 on a tree without the change under test): %v", f.name, err)
			}
			if strings.TrimSpace(string(want)) != string(gotJSON) {
				t.Errorf("flow %q changed without a call in it:\n--- want\n%s\n--- got\n%s", f.name, want, gotJSON)
			}
		})
	}
}
