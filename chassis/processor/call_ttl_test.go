package processor

import (
	"strconv"
	"testing"

	"github.com/tidwall/gjson"
)

// A call frame has a hop countdown of its own (frame.go), as it has its own
// transition history: a sub-flow pays for its own loops, never for the
// request's, and the request never for the sub-flow's. Depth bounds the
// total. A callee that uses its countdown up ends on the exhaustion, as
// the call's error; the caller's own countdown is untouched and it goes on.
func TestCallHasItsOwnHopBudget(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Conf.OpScopeTTLMax = 12
	seedScopes := func(stack string, n int) {
		for i := 1; i <= n; i++ {
			rule := `EMIT .n` + strconv.Itoa(i) + ` = true`
			if i == n {
				rule = `EMIT ._call.output.done = true, ._call.output.scopes = ` + strconv.Itoa(n)
			}
			seedOp(t, pu, stack, i*100, "s"+strconv.Itoa(i), rule)
		}
	}
	seedScopes("five", 5)
	seedScopes("many", 20)
	// Two calls of five scopes each, then a third scope: 3 + 5 + 5 hops
	// would be 13 of a shared 12; each frame's own 12 fits with room.
	seedOp(t, pu, "c", 100, "one", `WITH into = "_one" EXEC "call://five"`)
	seedOp(t, pu, "c", 200, "two", `WITH into = "_two" EXEC "call://five"`)
	seedOp(t, pu, "c", 300, "after", `EMIT .after = true`)
	final := callFinal(t, pu, callerIn, "c/0")
	for _, k := range []string{"_one", "_two"} {
		if !gjson.Get(final, k+".ok").Bool() || gjson.Get(final, k+".output.scopes").Int() != 5 {
			t.Errorf("%s = %s", k, gjson.Get(final, k).Raw)
		}
	}
	if !gjson.Get(final, "after").Bool() {
		t.Errorf("the caller's third scope did not run: %s", final)
	}

	// Twenty scopes in one callee: its countdown runs out inside it, the
	// call answers the exhaustion, and the caller goes on.
	seedOp(t, pu, "d", 100, "deep", `WITH into = "_deep" EXEC "call://many"`)
	seedOp(t, pu, "d", 200, "after", `EMIT .after = true`)
	final = callFinal(t, pu, `{"_txc":{"stack":"d","src":"http"}}`, "d/0")
	res := gjson.Get(final, "_deep")
	if res.Get("ok").Bool() || res.Get("error.code").String() != "txcl_scope_ttl_exhausted" || res.Get("output.done").Exists() {
		t.Errorf("_deep = %s", res.Raw)
	}
	if !gjson.Get(final, "after").Bool() {
		t.Errorf("the callee's exhaustion ended the caller: %s", final)
	}
}
