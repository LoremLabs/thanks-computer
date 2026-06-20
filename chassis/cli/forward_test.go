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
