package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOutdatedNotice(t *testing.T) {
	if got := OutdatedNotice("0.1.0", nil); got != "" {
		t.Errorf("nil policy: %q, want empty", got)
	}
	if got := OutdatedNotice("0.1.0", &Policy{}); got != "" {
		t.Errorf("empty minimum: %q, want empty", got)
	}
	if got := OutdatedNotice("0.2.0", &Policy{MinimumSupported: "0.2.0"}); got != "" {
		t.Errorf("equal: %q, want empty", got)
	}
	if got := OutdatedNotice("0.3.0", &Policy{MinimumSupported: "0.2.0"}); got != "" {
		t.Errorf("newer: %q, want empty", got)
	}
	if got := OutdatedNotice("0.1.0", &Policy{MinimumSupported: "0.2.0", Latest: "0.2.6"}); got == "" {
		t.Error("below minimum should warn")
	}
	if got := OutdatedNotice("dev", &Policy{MinimumSupported: "0.2.0"}); got != "" {
		t.Errorf("non-semver current: %q, want empty (safe)", got)
	}
	crit := OutdatedNotice("0.1.0", &Policy{MinimumSupported: "0.2.0", Critical: true})
	if !strings.Contains(crit, "CRITICAL") {
		t.Errorf("critical notice = %q, want CRITICAL", crit)
	}
}

func TestFetchServerInfo(t *testing.T) {
	var gotFormat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotFormat = r.URL.Query().Get("format")
		_ = json.NewEncoder(w).Encode(ServerInfo{
			Status:  "ok",
			Version: "0.1.0",
			Commit:  "abc1234",
			Chassis: "v0.2.4-0.x-281d46e",
			Client:  &Policy{Latest: "0.2.6", MinimumSupported: "0.2.0"},
		})
	}))
	defer srv.Close()

	info, err := FetchServerInfo(context.Background(), srv.URL, "txco-cli/test")
	if err != nil {
		t.Fatalf("FetchServerInfo: %v", err)
	}
	if gotFormat != "json" {
		t.Errorf("server saw format=%q, want json", gotFormat)
	}
	if info.Version != "0.1.0" || info.Chassis == "" {
		t.Errorf("build fields: %+v", info)
	}
	if info.Client == nil || info.Client.MinimumSupported != "0.2.0" {
		t.Errorf("policy: %+v", info.Client)
	}

	if _, err := FetchServerInfo(context.Background(), "", "txco-cli/test"); err == nil {
		t.Error("empty base URL should error")
	}
}
