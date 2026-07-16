package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLoopbackCallback(t *testing.T) {
	lb, err := startLoopback()
	if err != nil {
		t.Fatal(err)
	}
	defer lb.close()

	if !strings.HasPrefix(lb.url, "http://127.0.0.1:") {
		t.Fatalf("loopback url = %q, want 127.0.0.1", lb.url)
	}

	// Wrong state → 400 and NO result delivered (guards a stray localhost hit).
	resp, err := http.Get(lb.url + "?state=wrong&status=success")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong state: code = %d, want 400", resp.StatusCode)
	}

	// Correct state → 200 + return page + result delivered.
	resp, err = http.Get(lb.url + "?state=" + lb.state + "&status=success&session_id=cs_1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good state: code = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "complete") {
		t.Fatalf("return page = %q", string(body))
	}

	res, ok := lb.wait(2 * time.Second)
	if !ok || res.status != "success" || res.sessionID != "cs_1" {
		t.Fatalf("wait = %+v, ok = %v", res, ok)
	}
}

func TestReturnPageHTML(t *testing.T) {
	if !strings.Contains(returnPageHTML("success"), "complete") {
		t.Error("success page should read complete")
	}
	if !strings.Contains(returnPageHTML("cancel"), "canceled") {
		t.Error("cancel page should read canceled")
	}
}
