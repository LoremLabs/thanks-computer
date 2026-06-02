package admission

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestDrainFlag(t *testing.T) {
	t.Cleanup(func() { SetDraining(false) })
	if IsDraining() {
		t.Fatal("drain should be off by default")
	}
	SetDraining(true)
	if !IsDraining() {
		t.Fatal("SetDraining(true) should enable drain")
	}
	SetDraining(false)
	if IsDraining() {
		t.Fatal("SetDraining(false) should disable drain")
	}
}

func TestDrainResponseWeb(t *testing.T) {
	// A web request carries _txc.web.req; DrainResponse shapes a 503 web
	// response with Retry-After + Connection: close.
	in := `{"_txc":{"src":"http","web":{"req":{"method":"GET"}}}}`
	out := DrainResponse(in)
	if got := gjson.Get(out, "_txc.web.res.status").Int(); got != 503 {
		t.Errorf("status = %d, want 503", got)
	}
	if got := gjson.Get(out, "_txc.web.res.headers.retry-after.0").String(); got != "0" {
		t.Errorf("retry-after = %q, want 0", got)
	}
	if got := gjson.Get(out, "_txc.web.res.headers.connection.0").String(); got != "close" {
		t.Errorf("connection = %q, want close", got)
	}
	if !gjson.Get(out, "_txc.admission.denied").Bool() {
		t.Error("admission.denied marker should be set")
	}
}

func TestDrainResponseNonWeb(t *testing.T) {
	// A non-web envelope (no _txc.web.req) gets the marker but no web
	// response shaping (there's no HTTP writer to render it).
	in := `{"_txc":{"src":"cron"}}`
	out := DrainResponse(in)
	if gjson.Get(out, "_txc.web.res.status").Exists() {
		t.Error("non-web drain should not set a web response status")
	}
	if !gjson.Get(out, "_txc.admission.denied").Bool() {
		t.Error("admission.denied marker should be set even for non-web")
	}
}
