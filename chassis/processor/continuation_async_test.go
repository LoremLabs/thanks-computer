package processor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

func opWith(exec, meta string) operation.Operation {
	return operation.Operation{
		Name:      "research",
		Stack:     "website",
		Scope:     100,
		Meta:      meta,
		Resonator: &resonator.Resonator{Exec: exec},
	}
}

func TestIsAsyncOpClassification(t *testing.T) {
	cases := []struct {
		name string
		op   operation.Operation
		want bool
	}{
		{"https async", opWith("https://w.example/x", `{"mode":"async"}`), true},
		{"http async", opWith("http://w.example/x", `{"mode":"async"}`), true},
		{"https sync (no mode)", opWith("https://w.example/x", `{}`), false},
		{"https mode sync", opWith("https://w.example/x", `{"mode":"sync"}`), false},
		{"txco async ignored", opWith("txco://noop", `{"mode":"async"}`), false},
		{"stage jump", opWith("website/200", `{"mode":"async"}`), false},
		{"nil resonator", operation.Operation{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAsyncOp(tc.op); got != tc.want {
				t.Fatalf("isAsyncOp = %v, want %v", got, tc.want)
			}
		})
	}

	if !(&Unit{}).scopeHasAsync([]operation.Operation{
		opWith("txco://noop", `{}`),
		opWith("https://w/x", `{"mode":"async"}`),
	}) {
		t.Fatal("scopeHasAsync should be true when any op is async")
	}
	if (&Unit{}).scopeHasAsync([]operation.Operation{opWith("txco://noop", `{}`)}) {
		t.Fatal("scopeHasAsync should be false with no async ops")
	}
}

func TestOpIdentityAndCallbackURL(t *testing.T) {
	if got := opIdentity(operation.Operation{Name: "n", OpID: "id"}); got != "n" {
		t.Fatalf("opIdentity name = %q, want n", got)
	}
	if got := opIdentity(operation.Operation{OpID: "id"}); got != "id" {
		t.Fatalf("opIdentity fallback = %q, want id", got)
	}

	pu := &Unit{CallbackBaseURL: "https://chassis.example.com/"}
	if got := pu.callbackURLFor("opc_x"); got != "https://chassis.example.com/_txc/continuations/op/opc_x/complete" {
		t.Fatalf("callbackURLFor = %q", got)
	}
	pu2 := &Unit{Conf: config.Config{Fqdn: "host.local", WebAddr: ":8080"}}
	if got := pu2.callbackURLFor("opc_y"); got != "http://host.local:8080/_txc/continuations/op/opc_y/complete" {
		t.Fatalf("derived callbackURLFor = %q", got)
	}
}

func TestExecHTTPAsyncWrapsBodyAndToken(t *testing.T) {
	var gotBody []byte
	var gotToken, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotToken = r.Header.Get("X-Txco-Continuation-Token")
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-42"}`))
	}))
	defer srv.Close()

	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: config.Config{}}
	op := operation.Operation{Resonator: &resonator.Resonator{Exec: srv.URL}, Input: `{"topic":"x"}`}
	env := AsyncEnvelope{OpContinuationID: "opc_1", CallbackURL: "http://cb/x", RunID: "run_1", Stage: "website/100", Op: "research"}

	jobID, err := pu.ExecHTTPAsync(context.Background(), op, env, "secret-token")
	if err != nil {
		t.Fatalf("ExecHTTPAsync: %v", err)
	}
	if jobID != "job-42" {
		t.Fatalf("jobID = %q, want job-42", jobID)
	}
	if gotToken != "secret-token" {
		t.Fatalf("token header = %q", gotToken)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q", gotCT)
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &wrapped); err != nil {
		t.Fatalf("worker body not JSON: %v (%s)", err, gotBody)
	}
	if string(wrapped["input"]) != `{"topic":"x"}` {
		t.Fatalf("wrapped input = %s", wrapped["input"])
	}
	var gotEnv AsyncEnvelope
	if err := json.Unmarshal(wrapped["_txc"], &gotEnv); err != nil {
		t.Fatalf("_txc not an AsyncEnvelope: %v", err)
	}
	if gotEnv.OpContinuationID != "opc_1" || gotEnv.RunID != "run_1" || gotEnv.Op != "research" {
		t.Fatalf("envelope mismatch: %+v", gotEnv)
	}
}

func TestExecHTTPAsyncNon202IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // not 202
	}))
	defer srv.Close()
	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: config.Config{}}
	op := operation.Operation{Resonator: &resonator.Resonator{Exec: srv.URL}}
	if _, err := pu.ExecHTTPAsync(context.Background(), op, AsyncEnvelope{}, "t"); err == nil {
		t.Fatal("expected error on non-202 worker response")
	}
}

func TestFailPayloadIsJSON(t *testing.T) {
	var m map[string]map[string]string
	if err := json.Unmarshal(failPayload(`he said "hi"`), &m); err != nil {
		t.Fatalf("failPayload not JSON: %v", err)
	}
	if m["error"]["message"] != `he said "hi"` {
		t.Fatalf("message = %q", m["error"]["message"])
	}
}
