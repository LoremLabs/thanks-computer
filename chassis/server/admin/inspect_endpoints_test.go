package admin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestInspectEventPayload pins the inspect event envelope shape: src=inspect,
// the trusted tenant slug, and the stack/noun/id/args fields detectTenantBody
// and the tenant's _inspect ops read.
func TestInspectEventPayload(t *testing.T) {
	p := inspectEventPayload("acme", "marketing", "user", "matt@example.com",
		map[string]any{"window": "30d"})
	checks := map[string]string{
		"_txc.src":                 "inspect",
		"_txc.inspect.tenant":      "acme",
		"_txc.inspect.stack":       "marketing",
		"_txc.inspect.noun":        "user",
		"_txc.inspect.id":          "matt@example.com",
		"_txc.inspect.args.window": "30d",
	}
	for path, want := range checks {
		if got := gjson.Get(p, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

// Empty args must not leave an empty _txc.inspect.args object behind.
func TestInspectEventPayloadNoArgs(t *testing.T) {
	p := inspectEventPayload("acme", "marketing", "", "", nil)
	if gjson.Get(p, "_txc.inspect.args").Exists() {
		t.Errorf("args should be absent when empty, got %s", p)
	}
	if got := gjson.Get(p, "_txc.inspect.noun").String(); got != "" {
		t.Errorf("noun = %q, want empty (discovery form)", got)
	}
}

// inspectTestBus points the controller's processor bus at a channel the test
// owns and replies to each envelope with the given raw envelope JSON —
// standing in for a tenant _inspect stack.
func inspectTestBus(t *testing.T, c *Controller, replyRaw string) chan *event.Envelope {
	t.Helper()
	busCh := make(chan *event.Envelope, 1)
	c.pu.Bus = busCh
	go func() {
		env := <-busCh
		env.ResCh <- event.Payload{Raw: replyRaw, Type: event.JSON}
	}()
	return busCh
}

func postInspect(t *testing.T, c *Controller, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tenants/default/inspect",
		bytes.NewReader([]byte(body)))
	req = withTenantAdminCtx(req, "tenant_test")
	rr := httptest.NewRecorder()
	c.handleInspect(rr, req)
	return rr
}

// A card in the reply envelope is passed through verbatim with a 200.
func TestHandleInspectCardPassthrough(t *testing.T) {
	c := newTestController(t, config.Config{})
	inspectTestBus(t, c,
		`{"_txc":{"src":"inspect"},"_inspect":{"card":{"title":"Marketing Profile","sections":[{"title":"Signals","rows":[["Last activity","2026-07-02"],["Books finished",4]]}]}}}`)

	rr := postInspect(t, c, `{"stack":"marketing","noun":"user","id":"matt@example.com"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if got := gjson.Get(body, "card.title").String(); got != "Marketing Profile" {
		t.Errorf("card.title = %q, want Marketing Profile", got)
	}
	if got := gjson.Get(body, "card.sections.0.rows.1.1").Int(); got != 4 {
		t.Errorf("rows[1][1] = %d, want 4 (values must pass through untyped)", got)
	}
}

// The handler stamps the trusted tenant from the authenticated context, never
// the client body, and marks src=inspect.
func TestHandleInspectStampsTrustedTenant(t *testing.T) {
	c := newTestController(t, config.Config{})
	busCh := make(chan *event.Envelope, 1)
	c.pu.Bus = busCh
	var sent string
	go func() {
		env := <-busCh
		sent = env.Payload.Raw
		env.ResCh <- event.Payload{Raw: `{"_inspect":{"card":{"title":"x"}}}`, Type: event.JSON}
	}()

	rr := postInspect(t, c, `{"stack":"marketing"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := gjson.Get(sent, "_txc.inspect.tenant").String(); got != "default" {
		t.Errorf("_txc.inspect.tenant = %q, want the authenticated slug 'default'", got)
	}
	if got := gjson.Get(sent, "_txc.src").String(); got != "inspect" {
		t.Errorf("_txc.src = %q, want inspect", got)
	}
}

// No card in the reply (tenant has no matching inspector) → 404 no_inspector.
func TestHandleInspectNoCard(t *testing.T) {
	c := newTestController(t, config.Config{})
	inspectTestBus(t, c, `{"_txc":{"src":"inspect"}}`)

	rr := postInspect(t, c, `{"stack":"marketing","noun":"user"}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body: %s)", rr.Code, rr.Body.String())
	}
	if got := gjson.Get(rr.Body.String(), "error").String(); got != "no_inspector" {
		t.Errorf("error = %q, want no_inspector", got)
	}
}

// A missing stack is a 400 before anything reaches the bus.
func TestHandleInspectStackRequired(t *testing.T) {
	c := newTestController(t, config.Config{})
	rr := postInspect(t, c, `{"noun":"user"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rr.Code, rr.Body.String())
	}
}
