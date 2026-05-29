package ui

import (
	"strings"
	"testing"
)

// WaitPage must always return a usable, on-brand HTML document — the
// built single-file Svelte bundle when present, otherwise the inline
// no-build fallback. Both carry the CMY wordmark and a refresh path so
// the chassis never serves an empty/broken waiting response.
func TestWaitPage(t *testing.T) {
	html, built := WaitPage()
	if len(html) == 0 {
		t.Fatal("WaitPage returned empty html")
	}
	s := string(html)
	if !strings.Contains(s, "thanks, c") {
		t.Fatalf("WaitPage missing brand wordmark: %.120s", s)
	}
	if !strings.Contains(strings.ToLower(s), "<!doctype html>") {
		t.Fatalf("WaitPage not an HTML document: %.120s", s)
	}
	if built {
		// The Svelte single-file build inlines JS+CSS — it is large.
		if len(html) < 2000 {
			t.Fatalf("built page suspiciously small (%d bytes)", len(html))
		}
	} else {
		// Fallback must auto-refresh (no JS) so polling still works.
		if !strings.Contains(s, `http-equiv="refresh"`) {
			t.Fatalf("fallback page missing meta refresh")
		}
	}
}
