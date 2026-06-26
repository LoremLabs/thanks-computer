package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestHTTPErrorOriginTag(t *testing.T) {
	// A Cloudflare edge 502 carries cf-ray + Server: cloudflare and no JSON
	// code — the failure line must attribute it to the edge so a slow-but-OK
	// worker isn't mistaken for a worker crash.
	edge := &HTTPError{
		StatusCode: 502, Status: "502 Bad Gateway",
		Raw: "error code: 502", Server: "cloudflare", CFRay: "8f2abc-DFW",
	}
	got := edge.Error()
	for _, want := range []string{"502 Bad Gateway", "origin=cloudflare-edge", "cf-ray=8f2abc-DFW", "server=cloudflare"} {
		if !strings.Contains(got, want) {
			t.Errorf("edge 502 Error() = %q, want substring %q", got, want)
		}
	}

	// A genuine worker 5xx (database is locked) has no cf-ray → origin=worker.
	worker := &HTTPError{
		StatusCode: 500, Status: "500 Internal Server Error",
		Code: "create_stack", Detail: map[string]any{"err": "database is locked"},
	}
	got = worker.Error()
	if !strings.Contains(got, "origin=worker") {
		t.Errorf("worker 500 Error() = %q, want origin=worker", got)
	}
	if strings.Contains(got, "cloudflare") {
		t.Errorf("worker 500 Error() = %q, should not mention cloudflare", got)
	}
	if !strings.Contains(got, "database is locked") {
		t.Errorf("worker 500 Error() = %q, want the detail preserved", got)
	}

	// A 4xx the caller already understands gets NO origin tag (just noise).
	notFound := &HTTPError{StatusCode: 404, Status: "404 Not Found", Code: "file_not_found"}
	if got := notFound.Error(); strings.Contains(got, "[origin") {
		t.Errorf("404 Error() = %q, should have no origin tag", got)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"502 gateway", &HTTPError{StatusCode: 502}, true},
		{"500 worker", &HTTPError{StatusCode: 500}, true},
		{"503", &HTTPError{StatusCode: 503}, true},
		{"504", &HTTPError{StatusCode: 504}, true},
		{"429 too many", &HTTPError{StatusCode: 429}, true},
		{"404 fatal", &HTTPError{StatusCode: 404}, false},
		{"401 fatal", &HTTPError{StatusCode: 401}, false},
		{"400 fatal", &HTTPError{StatusCode: 400}, false},
		{"wrapped 503", fmt.Errorf("activate: %w", &HTTPError{StatusCode: 503}), true},
		{"network error", errors.New("dial tcp: connection refused"), true},
		{"context canceled", context.Canceled, false},
	}
	for _, tc := range cases {
		if got := IsRetryable(tc.err); got != tc.want {
			t.Errorf("%s: IsRetryable(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
