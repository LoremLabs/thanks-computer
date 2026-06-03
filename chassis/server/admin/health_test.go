package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// TestHandleHealthText: the default (no Accept/format) response stays the
// legacy plain-text probe so LB/uptime checks are unchanged.
func TestHandleHealthText(t *testing.T) {
	c := &Controller{pu: &processor.Unit{}}
	rec := httptest.NewRecorder()
	c.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok\n" {
		t.Errorf("body = %q, want \"ok\\n\"", rec.Body.String())
	}
}

// TestHandleHealthJSON: with JSON requested, the response carries build
// identity + the configured client policy, and status stays 200.
func TestHandleHealthJSON(t *testing.T) {
	c := &Controller{pu: &processor.Unit{Conf: config.Config{
		Build:                 config.BuildIdentity{Version: "0.1.0", Commit: "abc1234def", Chassis: "v0.2.4-0.x-281d46e"},
		ClientVersionLatest:   "0.2.6",
		ClientVersionMinimum:  "0.2.0",
		ClientVersionCritical: true,
	}}}

	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"accept", withAccept(httptest.NewRequest(http.MethodGet, "/healthz", nil), "application/json")},
		{"query", httptest.NewRequest(http.MethodGet, "/healthz?format=json", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.handleHealth(rec, tc.req)
			if rec.Code != http.StatusOK {
				t.Fatalf("code = %d, want 200", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("content-type = %q, want json", ct)
			}
			var resp struct {
				Status  string `json:"status"`
				Version string `json:"version"`
				Chassis string `json:"chassis"`
				Client  *struct {
					Latest           string `json:"latest"`
					MinimumSupported string `json:"minimum_supported"`
					Critical         bool   `json:"critical"`
				} `json:"client"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v\n%s", err, rec.Body.String())
			}
			if resp.Status != "ok" || resp.Version != "0.1.0" || resp.Chassis == "" {
				t.Errorf("build fields wrong: %+v", resp)
			}
			if resp.Client == nil || resp.Client.MinimumSupported != "0.2.0" || !resp.Client.Critical {
				t.Errorf("policy wrong: %+v", resp.Client)
			}
		})
	}
}

// TestHandleHealthJSONNoPolicy: a chassis with no policy configured omits the
// client block entirely (CLI treats absence as "no opinion").
func TestHandleHealthJSONNoPolicy(t *testing.T) {
	c := &Controller{pu: &processor.Unit{Conf: config.Config{Build: config.BuildIdentity{Version: "9.9.9"}}}}
	rec := httptest.NewRecorder()
	c.handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz?format=json", nil))
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if _, ok := resp["client"]; ok {
		t.Errorf("client block should be omitted when no policy set: %v", resp)
	}
}

func withAccept(r *http.Request, v string) *http.Request {
	r.Header.Set("Accept", v)
	return r
}
