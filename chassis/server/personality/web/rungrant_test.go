package web

import (
	"context"
	"net/http"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// A run grant's token on a request goes into the run's context and out of
// the headers the envelope will carry; every other header stays.
func TestIntakeRunGrant(t *testing.T) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set(processor.RunGrantHeader, "rg1.tok")
	ctx, out := intakeRunGrant(context.Background(), h)
	if got := processor.RunGrantFrom(ctx); got != "rg1.tok" {
		t.Errorf("context holds %q", got)
	}
	if out.Get(processor.RunGrantHeader) != "" || out.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", out)
	}
	if h.Get(processor.RunGrantHeader) != "rg1.tok" {
		t.Errorf("the request's own headers were changed")
	}
	plain := http.Header{"Accept": {"*/*"}}
	ctx, out = intakeRunGrant(context.Background(), plain)
	if processor.RunGrantFrom(ctx) != "" || out.Get("Accept") != "*/*" {
		t.Errorf("no token: ctx=%q headers=%v", processor.RunGrantFrom(ctx), out)
	}
}
