package processor

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	_ "github.com/loremlabs/thanks-computer/chassis/decide/vercel" // the real backend, for the live run only
)

// TestDecideLiveStack is the one live smoke test: a real rule calls the real
// gateway, and the next scope's WHEN acts on the typed answer —
//
//	state → ai://decide → normalized answer → WHEN
//
// It runs only with VERCEL_AI_KEY set and TXCO_LIVE_DECIDE=1 (so a
// stray key never makes the default run billable). The key reaches the op
// the way a dev chassis finds it: the env fallback.
func TestDecideLiveStack(t *testing.T) {
	if os.Getenv("VERCEL_AI_KEY") == "" || os.Getenv("TXCO_LIVE_DECIDE") != "1" {
		t.Skip("set VERCEL_AI_KEY and TXCO_LIVE_DECIDE=1 to call the real gateway")
	}
	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	pu.Conf.DecideZeroDataRetention = true
	pu.Conf.AIDefaultTimeout = "20s"
	pu.HTTPClient = &http.Client{Timeout: 20 * time.Second}

	stack := "decidelive/app"
	seedNamedOp(t, pu, stack, 0, "triage", `WHEN .ticket =~ /./
WITH provider  = "vercel",
     state     = .ticket,
     questions = &object(
       "refund", &object("type", "noul", "instructions", "Is the customer asking for money back?"),
       "route",  &object("type", "choice", "instructions", "Which team should handle this?",
                         "criteria", &object("billing", "charges and refunds", "technical", "bugs and outages")),
       "urgency", &object("type", "score", "instructions", "How urgent is this?",
                          "criteria", &array("low", "medium", "high"))),
     into      = "_triage",
     intent    = "live_smoke",
     timeout   = 20000
EXEC "ai://decide"`)
	seedNamedOp(t, pu, stack, 1, "route", `WHEN ._triage.ok == true && ._triage.answers.route.choice == "billing"
     && ._triage.answers.route.probability > 0.5
EMIT .team = "billing"`)
	seedNamedOp(t, pu, stack, 1, "fallback", `WHEN ._triage.ok != true
EMIT .team = "unrouted"`)

	out := runStage(t, context.Background(), pu,
		`{"ticket":"I was charged twice for my subscription this month. Please refund one of the charges."}`, stack+"/0")
	t.Logf("_triage = %s", gjson.Get(out, "_triage").Raw)
	if !gjson.Get(out, "_triage.ok").Bool() {
		t.Fatalf("decide failed: %s", gjson.Get(out, "_triage.error").Raw)
	}
	if got := gjson.Get(out, "team").String(); got != "billing" {
		t.Errorf("team = %q, want billing from the WHEN over the live answer", got)
	}
	for _, k := range []string{"refund", "route", "urgency"} {
		if !gjson.Get(out, "_triage.answers."+k).Exists() {
			t.Errorf("answer %q missing", k)
		}
	}
}
