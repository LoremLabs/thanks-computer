package processor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// TestExecHTTP_DialErrorMetaShape: a failed dial must report through
// the same {"error":[...]} meta shape as every other op error emitter.
// Regression: this block used to write a literal key named "error[0]"
// (sjson has no bracket syntax), which no consumer ever read.
func TestExecHTTP_DialErrorMetaShape(t *testing.T) {
	// grab a port that refuses connections by closing the server first
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := srv.URL
	client := srv.Client()
	srv.Close()

	pu := &Unit{Logger: zap.NewNop(), HTTPClient: client, Conf: config.Config{}}
	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: deadURL},
		Meta:      `{"method":"GET"}`,
		Input:     `{"_txc":{"src":"http"}}`,
	}
	payload, err := pu.ExecHTTP(context.Background(), op)
	if err == nil {
		t.Fatal("expected dial error, got nil")
	}
	if got := gjson.Get(payload.Meta, "error.0").String(); got != "dial-http-exec-err" {
		t.Errorf("meta error.0 = %q, want dial-http-exec-err (meta: %s)", got, payload.Meta)
	}
	if gjson.Get(payload.Meta, "errorMsg").String() == "" {
		t.Errorf("meta errorMsg missing (meta: %s)", payload.Meta)
	}
}
