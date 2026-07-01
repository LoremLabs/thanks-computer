package processor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// TestExecHTTP_URLOverride: a `WITH url` overrides the literal EXEC target with a
// runtime-built URL (the Stripe-by-customer-id GET the resync uses). The EXEC literal
// only routes the call to ExecHTTP; the override is what's actually fetched.
func TestExecHTTP_URLOverride(t *testing.T) {
	var gotTarget string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTarget = r.URL.Path + "?" + r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"status":"active"}]}`))
	}))
	defer srv.Close()

	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: config.Config{}}
	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: srv.URL}, // literal base — routes here
		Meta:      `{"method":"GET","url":"` + srv.URL + `/v1/subscriptions?customer=cus_123&limit=1"}`,
		Input:     `{}`,
	}
	if _, err := pu.ExecHTTP(context.Background(), op); err != nil {
		t.Fatalf("ExecHTTP: %v", err)
	}
	if gotTarget != "/v1/subscriptions?customer=cus_123&limit=1" {
		t.Errorf("override not used: server saw %q", gotTarget)
	}
}

// A non-http(s) override is rejected so it can't jump schemes / escape the
// egress-guarded client.
func TestExecHTTP_URLOverrideRejectsNonHTTP(t *testing.T) {
	pu := &Unit{Logger: zap.NewNop(), HTTPClient: http.DefaultClient, Conf: config.Config{}}
	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: "https://example.com"},
		Meta:      `{"url":"file:///etc/passwd"}`,
		Input:     `{}`,
	}
	if _, err := pu.ExecHTTP(context.Background(), op); err == nil {
		t.Error("expected error for non-http url override, got nil")
	}
}
