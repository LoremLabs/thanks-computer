package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A step's bodies come by their own request, once, under the same tenant as
// the trace, and the ref goes back exactly as it came.
func TestLoadStepBody(t *testing.T) {
	const ref = "traces/bodies/acme/1-rid.bodies#10-20"
	var path, gotRef string
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".json"):
			_, _ = w.Write([]byte(`{"rid":"rid","status":"ok","steps":[{"name":"think","scope":1,"status":"ok","duration_ms":1,"input_bytes":9,"output_bytes":9,"body_ref":"` + ref + `"},{"name":"gone","scope":2,"status":"ok","duration_ms":1,"input_bytes":9,"output_bytes":9,"body_ref":"missing"}]}`))
		case r.URL.Query().Get("ref") == "missing":
			http.Error(w, "body not found", http.StatusNotFound)
		default:
			calls++
			path, gotRef = r.URL.Path, r.URL.Query().Get("ref")
			_, _ = w.Write([]byte(`{"in":{"a":1},"out":{"b":2}}`))
		}
	}))
	defer srv.Close()

	c := New(Target{Addr: srv.URL, Tenant: "acme"})
	resp, _, err := c.GetTrace(context.Background(), "rid", true)
	if err != nil {
		t.Fatal(err)
	}
	step := &resp.Steps[0]
	if err := resp.LoadStepBody(context.Background(), step); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, "/traces/requests/rid/body") || !strings.Contains(path, "/acme/") {
		t.Errorf("path = %q", path)
	}
	if gotRef != ref {
		t.Errorf("ref = %q", gotRef)
	}
	if step.In == nil || step.Out == nil {
		t.Fatalf("step = %+v", step)
	}
	if err := resp.LoadStepBody(context.Background(), step); err != nil || calls != 1 {
		t.Errorf("a loaded step was fetched again: calls %d, err %v", calls, err)
	}

	if err := resp.LoadStepBody(context.Background(), &resp.Steps[1]); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("a body that never landed: %v", err)
	}

	// A step with no ref is left alone.
	if err := resp.LoadStepBody(context.Background(), &TraceStep{Name: "inline"}); err != nil {
		t.Errorf("no ref: %v", err)
	}
}
