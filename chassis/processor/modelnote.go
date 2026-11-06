package processor

import (
	"context"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// A model call's facts reach its trace step by a note: the ai:// handlers know
// them, and the step is recorded by dispatch's caller. The completion events
// carry the same facts, but only a file trace keeps those. On the step they
// survive every sink, and the summaries a trace is listed from.
type modelNote struct {
	set  bool
	seen trace.ModelFacts
}

var ctxKeyModelNote = ctxKeyType{name: "model-note"}

// withModelNote gives one dispatch of an ai:// op a place to leave its facts.
func withModelNote(ctx context.Context) (context.Context, *modelNote) {
	n := &modelNote{}
	return context.WithValue(ctx, ctxKeyModelNote, n), n
}

// noteModel leaves the facts of one call. A retried call leaves its last.
func noteModel(ctx context.Context, f trace.ModelFacts) {
	if n, ok := ctx.Value(ctxKeyModelNote).(*modelNote); ok {
		n.seen, n.set = f, true
	}
}

// facts is what the step carries. A call refused before it reached a provider
// (no key, a bad WITH) never gets as far as a completion event, and it is
// still a model call somebody will look for, so what it answered fills in.
func (n *modelNote) facts(exec string, payload event.Payload) *trace.ModelFacts {
	if n.set {
		f := n.seen
		return &f
	}
	kind, err := parseAISubOp(exec)
	if err != nil {
		kind = ""
	}
	f := trace.ModelFacts{Kind: kind}
	f.Provider = gjson.Get(payload.Raw, "_txc.chat.provider").String()
	f.Model = gjson.Get(payload.Raw, "_txc.chat.model").String()
	f.Routing = gjson.Get(payload.Raw, "_txc.chat.routing_decision").String()
	for _, path := range []string{"chat.error.code", "embed.error.code"} {
		if c := gjson.Get(payload.Raw, path).String(); c != "" {
			f.ErrorCode = c
		}
	}
	return &f
}
