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

// TestFormEncode covers the urlencoded serializer used by `WITH
// body_encoding="urlencoded"`: flat pairs, nested objects/arrays via
// bracket notation, RFC-3986 escaping (space->%20, @->%40), and the empty
// cases. Output is sorted, so expectations are deterministic.
func TestFormEncode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"flat", `{"a":"hi","b":" spaced"}`, `a=hi&b=%20spaced`},
		{"number", `{"quantity":1}`, `quantity=1`},
		{"nested-object", `{"metadata":{"email":"a@b.com"}}`, `metadata[email]=a%40b.com`},
		{
			"stripe-checkout",
			`{"mode":"subscription","line_items":[{"price":"price_1","quantity":1}],"metadata":{"email":"a@b.com"}}`,
			`line_items[0][price]=price_1&line_items[0][quantity]=1&metadata[email]=a%40b.com&mode=subscription`,
		},
		{"array-of-scalars", `{"x":["p","q"]}`, `x[0]=p&x[1]=q`},
		{"escape-reserved", `{"k":"a&b=c"}`, `k=a%26b%3Dc`},
		{"empty-object", `{}`, ``},
		{"empty-input", ``, ``},
		{"top-level-scalar-dropped", `"lonely"`, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formEncode([]byte(c.in)); got != c.want {
				t.Errorf("formEncode(%s) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestExecHTTP_URLEncodedBody: `WITH body_encoding="urlencoded",
// body_path="_form_input"` form-encodes that envelope object as the request
// body and sets the urlencoded content-type, while the JSON response still
// flows back (here nested under `into`). This is the shape the Stripe
// billing ops use.
func TestExecHTTP_URLEncodedBody(t *testing.T) {
	var gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"id":"cs_test_123","url":"https://checkout.stripe.com/c/pay/cs_test_123"}`))
	}))
	defer srv.Close()

	pu := &Unit{Logger: zap.NewNop(), HTTPClient: srv.Client(), Conf: config.Config{}}
	op := operation.Operation{
		Resonator: &resonator.Resonator{Exec: srv.URL},
		Meta:      `{"method":"POST","body_encoding":"urlencoded","body_path":"_form_input","into":"_stripe"}`,
		Input:     `{"_form_input":{"mode":"subscription","metadata":{"email":"a@b.com"}},"_txc":{"src":"http"}}`,
	}
	payload, err := pu.ExecHTTP(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecHTTP: %v", err)
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("content-type = %q, want application/x-www-form-urlencoded", gotCT)
	}
	want := "metadata[email]=a%40b.com&mode=subscription"
	if string(gotBody) != want {
		t.Errorf("body = %q, want %q", gotBody, want)
	}
	if !strings.Contains(payload.Raw, `"_stripe":{`) ||
		!strings.Contains(payload.Raw, `"url":"https://checkout.stripe.com`) {
		t.Errorf("response not nested under _stripe: %s", payload.Raw)
	}
}
