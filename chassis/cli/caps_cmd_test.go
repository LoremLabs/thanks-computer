package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func capsStub(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/caps") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const capsStubBody = `{"count":3,"caps":[` +
	`{"name":"broken","stack":"loop","entry":0,"stage":"","err":"capability declaration: entry is required"},` +
	`{"name":"local.web.fetch","stack":"pony-web","entry":0,"stage":"pony-web/0","description":"Fetch a page.\nSecond line.","input":{"url":{"required":true}}},` +
	`{"name":"mail.send","stack":"loop","entry":7000,"stage":"loop/7000","description":"Send a message.","input":{"to":{"required":true},"subject":{},"message":{"required":true}},"timeout":60000}]}`

func TestRunCapsListTable(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	srv := capsStub(t, capsStubBody)

	var out, errb bytes.Buffer
	if code := runCaps([]string{"list", "--addr", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d; stderr=%q", code, errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "NAME") {
		t.Fatalf("table:\n%s", out.String())
	}
	for i, want := range [][]string{
		{"broken", "loop", "BROKEN: capability declaration: entry is required"},
		{"local.web.fetch", "pony-web/0", "url*", "Fetch a page."},
		{"mail.send", "loop/7000", "60000ms", "message*,subject,to*", "Send a message."},
	} {
		for _, w := range want {
			if !strings.Contains(lines[i+1], w) {
				t.Errorf("line %d lacks %q: %s", i+1, w, lines[i+1])
			}
		}
	}
	if strings.Contains(out.String(), "Second line") {
		t.Errorf("only the description's first line belongs in the table:\n%s", out.String())
	}

	// A tenant with none says so.
	out.Reset()
	if code := runCaps([]string{"list", "--addr", capsStub(t, `{"count":0,"caps":[]}`).URL}, &out, &errb); code != 0 || !strings.Contains(out.String(), "no capabilities declared") {
		t.Fatalf("empty: exit=%d %q", code, out.String())
	}
}

func TestRunCapsListJSON(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	srv := capsStub(t, capsStubBody)

	var out, errb bytes.Buffer
	if code := runCaps([]string{"list", "--addr", srv.URL, "--json"}, &out, &errb); code != 0 {
		t.Fatalf("exit=%d; stderr=%q", code, errb.String())
	}
	var got []struct {
		Name  string `json:"name"`
		Stack string `json:"stack"`
		Stage string `json:"stage"`
		Input map[string]struct {
			Required bool `json:"required"`
		} `json:"input"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out.String())
	}
	if len(got) != 3 || got[1].Name != "local.web.fetch" || got[1].Stage != "pony-web/0" || !got[1].Input["url"].Required {
		t.Errorf("got %+v", got)
	}

	// None: an empty array, not null.
	out.Reset()
	if code := runCaps([]string{"list", "--addr", capsStub(t, `{"count":0,"caps":[]}`).URL, "--json"}, &out, &errb); code != 0 || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("empty: exit=%d %q", code, out.String())
	}
}

func TestRunCapsUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runCaps(nil, &out, &errb); code != 2 {
		t.Errorf("no command: exit=%d", code)
	}
	if code := runCaps([]string{"nope"}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "unknown command") {
		t.Errorf("unknown command: exit=%d %q", code, errb.String())
	}
}
