package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// TestExecHTTP_GETMethodNoBody: `WITH method="GET"` issues a GET with no
// request body (so an op can call third-party GET APIs whose params are
// in the URL), and the JSON response flows back.
func TestExecHTTP_GETMethodNoBody(t *testing.T) {
	var gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"dateTime":"2026-01-01T00:00:00","timeZone":"Europe/London"}`))
	}))
	defer srv.Close()

	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: config.Config{}}
	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: srv.URL + "/api/Time/current/zone?timeZone=Europe/London"},
		Meta:      `{"method":"GET"}`,
		Input:     `{"_txc":{"src":"http"}}`,
	}
	payload, err := pu.ExecHTTP(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecHTTP: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("server saw method %q, want GET", gotMethod)
	}
	if len(gotBody) != 0 {
		t.Errorf("GET should send no body, got %q", gotBody)
	}
	if !strings.Contains(payload.Raw, `"dateTime"`) {
		t.Errorf("response not returned: %s", payload.Raw)
	}
}

// TestExecHTTP_IntoNests: `WITH into="london"` wraps the response under
// that key so two calls can merge cleanly instead of colliding.
func TestExecHTTP_IntoNests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"x":1,"y":"z"}`))
	}))
	defer srv.Close()

	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: config.Config{}}
	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: srv.URL},
		Meta:      `{"method":"GET","into":"london"}`,
		Input:     `{}`,
	}
	payload, err := pu.ExecHTTP(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecHTTP: %v", err)
	}
	if !strings.Contains(payload.Raw, `"london":{"x":1,"y":"z"}`) {
		t.Errorf("into nesting failed: %s", payload.Raw)
	}
}

// TestExecHTTP_DefaultPostKeepsBody: with no method, the legacy behavior
// holds — POST with the envelope as the body.
func TestExecHTTP_DefaultPostKeepsBody(t *testing.T) {
	var gotMethod string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: config.Config{}}
	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: srv.URL},
		Input:     `{"hello":"world"}`,
	}
	if _, err := pu.ExecHTTP(context.Background(), op); err != nil {
		t.Fatalf("ExecHTTP: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("default method = %q, want POST", gotMethod)
	}
	if !strings.Contains(string(gotBody), `"hello":"world"`) {
		t.Errorf("POST should send the envelope body, got %q", gotBody)
	}
}

func TestNormalizeEnvelopePath(t *testing.T) {
	cases := map[string]string{
		"london":  "london",
		".london": "london",
		"":        "",
		"a.b":     "a.b",
		"@x":      "_txc.x",
	}
	for in, want := range cases {
		if got := normalizeEnvelopePath(in); got != want {
			t.Errorf("normalizeEnvelopePath(%q) = %q, want %q", in, got, want)
		}
	}
}
