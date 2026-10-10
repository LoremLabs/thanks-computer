package web

import (
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestApplyNegotiation(t *testing.T) {
	const link = `</about.md>; rel="alternate"; type="text/markdown"`
	for _, c := range []struct {
		name     string
		output   string
		wantVary []string
		wantLink []string
	}{
		{
			name:   "no marks: nothing added",
			output: `{"_txc":{"web":{"res":{"headers":{"content-type":["text/html"]}}}}}`,
		},
		{
			name:     "html answer: vary and link",
			output:   `{"_txc":{"web":{"negotiated":{"vary":"Accept","alternate":"/about.md"},"res":{"headers":{"content-type":["text/html; charset=utf-8"]}}}}}`,
			wantVary: []string{"Accept"},
			wantLink: []string{link},
		},
		{
			name:     "markdown answer: vary, no link",
			output:   `{"_txc":{"web":{"negotiated":{"vary":"Accept","alternate":"/about.md"},"res":{"headers":{"content-type":["text/markdown; charset=utf-8"]}}}}}`,
			wantVary: []string{"Accept"},
		},
		{
			name:     "an existing Vary is kept, Accept added",
			output:   `{"_txc":{"web":{"negotiated":{"vary":"Accept"},"res":{"headers":{"Vary":["Origin"],"content-type":["text/html"]}}}}}`,
			wantVary: []string{"Origin", "Accept"},
		},
		{
			name:     "Vary already names accept",
			output:   `{"_txc":{"web":{"negotiated":{"vary":"Accept"},"res":{"headers":{"vary":["Origin, accept"]}}}}}`,
			wantVary: []string{"Origin, accept"},
		},
		{
			name:     "Vary: * covers it",
			output:   `{"_txc":{"web":{"negotiated":{"vary":"Accept"},"res":{"headers":{"vary":["*"]}}}}}`,
			wantVary: []string{"*"},
		},
		{
			name:     "a lone-string Vary becomes a list",
			output:   `{"_txc":{"web":{"negotiated":{"vary":"Accept"},"res":{"headers":{"vary":"Origin"}}}}}`,
			wantVary: []string{"Origin", "Accept"},
		},
		{
			name:     "an existing Link is kept",
			output:   `{"_txc":{"web":{"negotiated":{"vary":"Accept","alternate":"/about.md"},"res":{"headers":{"Content-Type":["text/html"],"link":["</s.css>; rel=preload"]}}}}}`,
			wantVary: []string{"Accept"},
			wantLink: []string{"</s.css>; rel=preload", link},
		},
		{
			name:     "no content type set: not HTML",
			output:   `{"_txc":{"web":{"negotiated":{"vary":"Accept","alternate":"/about.md"}}}}`,
			wantVary: []string{"Accept"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeResHeaders(w.Header(), applyNegotiation(c.output))
			if got := w.Header().Values("Vary"); !reflect.DeepEqual(got, c.wantVary) && !(len(got) == 0 && len(c.wantVary) == 0) {
				t.Errorf("Vary = %q, want %q", got, c.wantVary)
			}
			if got := w.Header().Values("Link"); !reflect.DeepEqual(got, c.wantLink) && !(len(got) == 0 && len(c.wantLink) == 0) {
				t.Errorf("Link = %q, want %q", got, c.wantLink)
			}
		})
	}
}
