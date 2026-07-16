package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSplitForwardFlags(t *testing.T) {
	cases := []struct {
		name        string
		args        []string
		wantGlobals map[string]string
		wantRest    []string
	}{
		{
			name:        "profile space form extracted, positionals kept",
			args:        []string{"grant", "show", "prod-mankins", "--profile", "txco-production"},
			wantGlobals: map[string]string{"profile": "txco-production"},
			wantRest:    []string{"grant", "show", "prod-mankins"},
		},
		{
			name:        "profile equals form",
			args:        []string{"grant", "show", "x", "--profile=prod"},
			wantGlobals: map[string]string{"profile": "prod"},
			wantRest:    []string{"grant", "show", "x"},
		},
		{
			name:        "global flag in the middle",
			args:        []string{"grant", "--addr", "https://h:1", "show", "x"},
			wantGlobals: map[string]string{"addr": "https://h:1"},
			wantRest:    []string{"grant", "show", "x"},
		},
		{
			name:        "command-specific flags are preserved verbatim",
			args:        []string{"foo", "--bar", "baz", "--profile", "p"},
			wantGlobals: map[string]string{"profile": "p"},
			wantRest:    []string{"foo", "--bar", "baz"},
		},
		{
			name:        "no flags",
			args:        []string{"grant", "show", "x"},
			wantGlobals: map[string]string{},
			wantRest:    []string{"grant", "show", "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, r := splitForwardFlags(tc.args)
			if !reflect.DeepEqual(g, tc.wantGlobals) {
				t.Errorf("globals = %v, want %v", g, tc.wantGlobals)
			}
			if !reflect.DeepEqual(r, tc.wantRest) {
				t.Errorf("rest = %v, want %v", r, tc.wantRest)
			}
		})
	}
}

// TestForwardToServerPollLoop: a POLLABLE command (poll_after_ms > 0) makes the
// forwarder print, re-run with the returned cursor, and stop once poll_after_ms
// drops to 0 — cursor threading visible in the output.
func TestForwardToServerPollLoop(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir()) // no real profile/signer → unsigned request

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Args   []string `json:"args"`
			Cursor string   `json:"cursor"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		calls++
		resp := map[string]any{
			"stdout": fmt.Sprintf("poll%d cursor=%q\n", calls, req.Cursor),
			"exit":   0,
		}
		if calls < 3 { // two more polls, then stop
			resp["cursor"] = fmt.Sprintf("cur%d", calls)
			resp["poll_after_ms"] = 1
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	status, ok := forwardToServer("nats", []string{"sub", "x", "--addr", srv.URL}, &out, &errb)
	if !ok || status != 0 {
		t.Fatalf("status=%d ok=%v stderr=%q", status, ok, errb.String())
	}
	if calls != 3 {
		t.Fatalf("server calls = %d, want 3 (loop until poll_after_ms==0)", calls)
	}
	s := out.String()
	if !strings.Contains(s, `poll1 cursor=""`) ||
		!strings.Contains(s, `poll2 cursor="cur1"`) ||
		!strings.Contains(s, `poll3 cursor="cur2"`) {
		t.Fatalf("cursor not threaded across polls:\n%s", s)
	}
}

// TestForwardToServerTenantFallback: a command the super-admin /v1/cli doesn't
// implement (404) is retried against the tenant-scoped /v1/tenants/{t}/cli when a
// tenant is resolvable — the path self-serve verbs like `credits buy` take.
func TestForwardToServerTenantFallback(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir()) // no real profile/signer → unsigned request

	var adminHits, tenantHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/cli":
			adminHits++
			w.WriteHeader(http.StatusNotFound) // admin registry doesn't have it
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown_command"})
		case "/v1/tenants/acme/cli":
			tenantHits++
			_ = json.NewEncoder(w).Encode(map[string]any{"stdout": "bought\n", "exit": 0})
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	status, ok := forwardToServer("credits", []string{"buy", "1", "--addr", srv.URL, "--tenant", "acme"}, &out, &errb)
	if !ok || status != 0 {
		t.Fatalf("status=%d ok=%v stderr=%q", status, ok, errb.String())
	}
	if adminHits != 1 || tenantHits != 1 {
		t.Fatalf("adminHits=%d tenantHits=%d, want 1/1 (admin 404 → tenant fallback)", adminHits, tenantHits)
	}
	if out.String() != "bought\n" {
		t.Fatalf("out=%q, want bought", out.String())
	}
}

// TestForwardToServerNoTenantNoFallback: with no tenant resolvable... is not
// reachable here because ResolveTenant always falls back to the default tenant
// slug; the admin-first ordering means a command the admin endpoint DOES answer
// never reaches the tenant endpoint (covered by SingleShot). This documents that
// admin-first keeps legacy behaviour: a 404-only server falls through unchanged.
func TestForwardToServerBothUnsupportedFallsThrough(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // neither endpoint implements it
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "unknown_command"})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	status, ok := forwardToServer("nope", []string{"--addr", srv.URL, "--tenant", "acme"}, &out, &errb)
	if ok || status != 0 {
		t.Fatalf("status=%d ok=%v, want 0/false (fall through to unknown-subcommand)", status, ok)
	}
}

// TestForwardToServerSingleShot: a non-pollable command (no poll directive) runs
// exactly once — the classic behaviour is unchanged.
func TestForwardToServerSingleShot(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{"stdout": "done\n", "exit": 0})
	}))
	defer srv.Close()

	var out, errb bytes.Buffer
	status, ok := forwardToServer("credit", []string{"grant", "--addr", srv.URL}, &out, &errb)
	if !ok || status != 0 || calls != 1 || out.String() != "done\n" {
		t.Fatalf("status=%d ok=%v calls=%d out=%q", status, ok, calls, out.String())
	}
}
