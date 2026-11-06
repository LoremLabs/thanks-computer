package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// The link between the two halves of a run was only in --json, which is how a
// resume trace came to look as if it came from nowhere.
func TestPrintSummaryShowsWhereATraceBelongs(t *testing.T) {
	var b bytes.Buffer
	printSummary(&b, &client.TraceResponse{
		RID:  "resume-run_x-loop_502",
		Tags: map[string]string{"run": "run_abc", "pony": "eager.moss"},
		Continuation: &client.TraceContinuation{
			Self: "resume", OriginRID: "CgmknSVYfoW5x8kQ3o5N3",
			Resumes: []client.TraceResume{{RID: "resume-run_x-loop_502", Stage: "loop/502"}},
		},
	})
	out := b.String()
	if !strings.Contains(out, "origin   CgmknSVYfoW5x8kQ3o5N3") {
		t.Errorf("no origin: %s", out)
	}
	if strings.Contains(out, "resume   resume-run_x-loop_502") {
		t.Errorf("a trace listed itself as its own resume: %s", out)
	}
	if !strings.Contains(out, "tags     pony=eager.moss run=run_abc") {
		t.Errorf("tags are missing or unsorted: %s", out)
	}

	b.Reset()
	printSummary(&b, &client.TraceResponse{
		RID: "origin-rid",
		Continuation: &client.TraceContinuation{
			Self: "origin", OriginRID: "origin-rid",
			Resumes: []client.TraceResume{{RID: "resume-x", Stage: "loop/502"}},
		},
	})
	if out := b.String(); !strings.Contains(out, "resume   resume-x (loop/502)") || strings.Contains(out, "origin   ") {
		t.Errorf("origin trace: %s", out)
	}
}

// A reader answers steps in its own order. Shown as they ran, a call's steps
// sit between its caller's, and fractions of a second of different widths
// still sort as times.
func TestPrintStepsShowsThemAsTheyRan(t *testing.T) {
	var b bytes.Buffer
	printSteps(&b, []client.TraceStep{
		{Stack: "acme", Scope: 200, Name: "render", StartedAt: "2026-11-06T13:30:22.1Z"},
		{Stack: "agent", Scope: 100, Name: "think", StartedAt: "2026-11-06T13:30:14.84Z"},
		{Stack: "agent", Scope: 200, Name: "after", StartedAt: "2026-11-06T13:30:14.9Z"},
		{Stack: "acme", Scope: 100, Name: "dispatch", StartedAt: "2026-11-06T13:30:14.5Z"},
	})
	out := b.String()
	last := -1
	for _, name := range []string{"0100-dispatch", "0100-think", "0200-after", "0200-render"} {
		i := strings.Index(out, name)
		if i < 0 || i < last {
			t.Fatalf("%s out of order: %s", name, out)
		}
		last = i
	}
}

func TestOperationCellNamesTheModel(t *testing.T) {
	cases := map[string]client.TraceStep{
		"txco://kv/get": {Operation: "txco://kv/get"},
		"ai://chat gpt-5.4 13973→33": {Operation: "ai://chat",
			Model: &client.TraceModel{Kind: "chat", Model: "gpt-5.4", TokensIn: 13973, TokensOut: 33}},
		"ai://embed small 25": {Operation: "ai://embed",
			Model: &client.TraceModel{Kind: "embed", Model: "small", TokensIn: 25}},
		"ai://chat txco_chat_no_key": {Operation: "ai://chat",
			Model: &client.TraceModel{Kind: "chat", ErrorCode: "txco_chat_no_key"}},
	}
	for want, s := range cases {
		if got := operationCell(s); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// A step names the version its rule came from, when that is known.
func TestStepWhereNamesTheVersion(t *testing.T) {
	if got := stepWhere(&client.TraceStep{Stack: "loop", Scope: 7100, StackVersion: 116}); got != "loop/7100 (v116)" {
		t.Errorf("got %q", got)
	}
	if got := stepWhere(&client.TraceStep{Stack: "loop", Scope: 7100}); got != "loop/7100" {
		t.Errorf("got %q", got)
	}
}
