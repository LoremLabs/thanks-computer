package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// TestExecHTTP_JSONBodyPath: `WITH body_path` without an encoding sends that
// one value of the envelope as the JSON body, exactly — no `_txc`, none of
// the rule's other keys — with the method asked for; a path with nothing at
// it sends no request at all.
func TestExecHTTP_JSONBodyPath(t *testing.T) {
	var gotMethod, gotCT string
	var gotBody []byte
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotMethod, gotCT = r.Method, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7}`))
	}))
	defer srv.Close()
	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: config.Config{}}

	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: srv.URL},
		Meta:      `{"method":"PATCH","body_path":"_abd.body","into":"_res","status_into":"_st"}`,
		Input:     `{"_abd":{"body":{"title":"Say the city","labels":["docs"]},"url":"x"},"_txc":{"src":"cap"}}`,
	}
	payload, err := pu.ExecHTTP(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecHTTP: %v", err)
	}
	if gotMethod != "PATCH" || gotCT != "application/json" {
		t.Errorf("method %q content-type %q", gotMethod, gotCT)
	}
	if want := `{"title":"Say the city","labels":["docs"]}`; string(gotBody) != want {
		t.Errorf("body = %s, want %s", gotBody, want)
	}
	if want := `{"_st":201,"_res":{"number":7}}`; payload.Raw != want {
		t.Errorf("answer = %s, want %s", payload.Raw, want)
	}

	op.Meta = `{"method":"POST","body_path":"_abd.nothing"}`
	if _, err := pu.ExecHTTP(context.Background(), op); err == nil {
		t.Error("a body_path with nothing at it: want an error")
	}
	if hits != 1 {
		t.Errorf("requests = %d, want 1 (none for the missing path)", hits)
	}
}
