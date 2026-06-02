package cli

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

func TestIs403(t *testing.T) {
	if !is403(&client.HTTPError{StatusCode: http.StatusForbidden}) {
		t.Fatal("403 not detected")
	}
	if is403(&client.HTTPError{StatusCode: http.StatusNotFound}) {
		t.Fatal("404 wrongly detected as 403")
	}
	if is403(errors.New("boom")) {
		t.Fatal("plain error wrongly detected as 403")
	}
}

func TestRequestErrorMessage(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("TXCO_PROFILE", "")
	t.Setenv("TXCO_PRIVATE_KEY_PATH", "")

	// Non-403: just the formatted error, no whoami round-trip.
	if got := requestErrorMessage("admin resync", client.Target{}, "", errors.New("boom")); got != "admin resync: boom" {
		t.Fatalf("non-403 message = %q", got)
	}

	// 403: names the local active profile AND the chassis's view of it (whoami).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/whoami" {
			_ = json.NewEncoder(w).Encode(client.WhoamiResponse{
				Source: "signed", ActorID: "actor_x", Label: "matt", SuperAdmin: false,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	he := &client.HTTPError{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Code: "capability_denied"}
	msg := requestErrorMessage("admin resync", client.Target{Addr: srv.URL}, "myprofile", he)
	for _, want := range []string{"myprofile", "actor_x", "super_admin=false"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("403 message missing %q: %q", want, msg)
		}
	}
}
