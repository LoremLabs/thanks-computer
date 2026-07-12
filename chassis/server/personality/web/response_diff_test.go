package web

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Frozen copies of the pre-optimization response projection. The
// optimized paths must be byte-identical (envelope) / behavior-identical
// (headers written) for every input.

func checkStatusFrozen(output string) (string, int) {
	var status int
	st, err := strconv.ParseInt(gjson.Get(output, "_txc.web.res.status").String(), 10, 64)
	if (err != nil) || (st < 100) || (st > 599) {
		st = 200
	}
	status = int(st)
	output, _ = sjson.Set(output, "_txc.web.res.status", status)
	return output, status
}

func checkContentTypeFrozen(output string) string {
	ct := gjson.Get(output, "_txc.web.res.headers.content-type.0").String()
	if ct == "" {
		ct = "application/json"
	}
	output, _ = sjson.Set(output, "_txc.web.res.headers.content-type.0", ct)
	return output
}

func applyAdmissionFrozen(output string) string {
	if !gjson.Get(output, "_txc.admission.denied").Bool() {
		return output
	}
	if gjson.Get(output, "_txc.web.res.status").Exists() {
		return output
	}
	status := int(gjson.Get(output, "_txc.admission.status").Int())
	if status < 100 || status > 599 {
		status = http.StatusForbidden
	}
	output, _ = sjson.Set(output, "_txc.web.res.status", status)
	output, _ = sjson.Set(output, "_txc.web.res.headers.content-type.0", "text/plain; charset=utf-8")
	if reason := gjson.Get(output, "_txc.admission.reason").String(); reason != "" {
		output, _ = sjson.Set(output, "_txc.web.res.headers.x-txc-deny-reason.0", reason)
	}
	if ra := gjson.Get(output, "_txc.admission.retry_after"); ra.Exists() {
		output, _ = sjson.Set(output, "_txc.web.res.headers.retry-after.0", strconv.Itoa(int(ra.Int())))
	}
	if status == http.StatusServiceUnavailable {
		if !gjson.Get(output, "_txc.admission.retry_after").Exists() {
			output, _ = sjson.Set(output, "_txc.web.res.headers.retry-after.0", "0")
		}
		output, _ = sjson.Set(output, "_txc.web.res.headers.connection.0", "close")
	}
	body := strconv.Itoa(status) + " " + http.StatusText(status) + "\n"
	output, _ = sjson.Set(output, "_txc.web.res.body", base64Encode(body))
	return output
}

func base64Encode(s string) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], data[i:])
		b.WriteByte(tbl[chunk[0]>>2])
		b.WriteByte(tbl[(chunk[0]&0x3)<<4|chunk[1]>>4])
		if n > 1 {
			b.WriteByte(tbl[(chunk[1]&0xF)<<2|chunk[2]>>6])
		} else {
			b.WriteByte('=')
		}
		if n > 2 {
			b.WriteByte(tbl[chunk[2]&0x3F])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}

var responseDiffDocs = []string{
	`{}`,
	`{"ok":true}`,
	`{"_txc":{"web":{"res":{"status":200}}}}`,
	`{"_txc":{"web":{"res":{"status":304,"headers":{"content-type":["image/jpeg"]}}}},"x":1}`,
	`{"_txc":{"web":{"res":{"status":"200"}}}}`,
	`{"_txc":{"web":{"res":{"status":200.0}}}}`,
	`{"_txc":{"web":{"res":{"status":42}}}}`,
	`{"_txc":{"web":{"res":{"status":700}}}}`,
	`{"_txc":{"web":{"res":{"headers":{"content-type":["text/html; charset=utf-8"]}}}}}`,
	`{"_txc":{"web":{"res":{"headers":{"content-type":[""]}}}}}`,
	`{"_txc":{"web":{"res":{"headers":{"content-type":["uni é🎈"]}}}}}`,
	`{"_txc":{"admission":{"denied":true,"status":429,"reason":"rate","retry_after":2}}}`,
	`{"_txc":{"admission":{"denied":true,"status":503}}}`,
	`{"_txc":{"admission":{"denied":true,"status":9999}}}`,
	`{"_txc":{"admission":{"denied":true,"status":429}},"_txc2":1}`,
	`{"_txc":{"admission":{"denied":false}}}`,
	`{"_txc":{"admission":{"denied":true,"status":403}},"_txc":{"web":{"res":{"status":418}}}}`,
	`{ "_txc": {"web": {"res": {"status": 200}}} }`,
}

func TestCheckStatusMatchesFrozen(t *testing.T) {
	for _, doc := range responseDiffDocs {
		wantDoc, wantStatus := checkStatusFrozen(doc)
		gotDoc, gotStatus := checkStatus(doc)
		if gotDoc != wantDoc || gotStatus != wantStatus {
			t.Fatalf("checkStatus mismatch for %s\nwant %q %d\ngot  %q %d", doc, wantDoc, wantStatus, gotDoc, gotStatus)
		}
	}
}

func TestCheckContentTypeMatchesFrozen(t *testing.T) {
	for _, doc := range responseDiffDocs {
		want := checkContentTypeFrozen(doc)
		got := checkContentType(doc)
		if got != want {
			t.Fatalf("checkContentType mismatch for %s\nwant %q\ngot  %q", doc, want, got)
		}
	}
}

func TestApplyAdmissionMatchesFrozen(t *testing.T) {
	for _, doc := range responseDiffDocs {
		want := applyAdmissionFrozen(doc)
		got := applyAdmission(doc)
		if got != want {
			t.Fatalf("applyAdmission mismatch for %s\nwant %q\ngot  %q", doc, want, got)
		}
	}
}

// TestStripUnderscoreMatchesSlow drives the single-pass strip against
// the frozen Delete loop over randomized docs (compact and pretty,
// plain and hostile keys).
func TestStripUnderscoreMatchesSlow(t *testing.T) {
	r := rand.New(rand.NewSource(20260712))
	keys := []string{
		"_txc", "_files", "_ts", "_embed", "ok", "data", "text",
		"items", "_txc2", "a", "b", "user name", "_x-y", "_a.b",
		"_wild*", "with.dot", "_",
	}
	fastHits := 0
	for i := 0; i < 8000; i++ {
		var b strings.Builder
		pretty := r.Intn(4) == 0
		sep, colon := ",", ":"
		if pretty {
			sep, colon = ", ", ": "
		}
		b.WriteByte('{')
		n := r.Intn(6)
		for j := 0; j < n; j++ {
			if j > 0 {
				b.WriteString(sep)
			}
			kb, _ := json.Marshal(keys[r.Intn(len(keys))])
			b.Write(kb)
			b.WriteString(colon)
			switch r.Intn(4) {
			case 0:
				b.WriteString(`{"nested":{"deep":[1,2]}}`)
			case 1:
				b.WriteString(strconv.Itoa(r.Intn(1000)))
			case 2:
				b.WriteString(`"str"`)
			default:
				b.WriteString("null")
			}
		}
		b.WriteByte('}')
		doc := b.String()

		want := stripTopLevelUnderscoreSlow(doc)
		var got string
		if fast, ok := stripTopLevelUnderscoreFast(doc); ok {
			got = fast
			fastHits++
		} else {
			got = stripTopLevelUnderscoreSlow(doc)
		}
		if got != want {
			t.Fatalf("iter %d strip mismatch for %s\nwant %q\ngot  %q", i, doc, want, got)
		}
	}
	if fastHits == 0 {
		t.Fatal("fast strip never engaged")
	}
	t.Logf("fast strip hit rate: %d/8000", fastHits)
}

// TestApplyResponseHeadHeaders pins the header fan-out behavior: array
// values set repeatedly (last wins per Set), scalar values set once.
// Note: header NAMES containing '.' are now written too — the old
// re-resolve-by-path form silently dropped them; that is a deliberate
// behavior fix, not a regression.
func TestApplyResponseHeadHeaders(t *testing.T) {
	doc := `{"_txc":{"web":{"res":{"status":201,"headers":{"content-type":["text/plain"],"x-multi":["a","b"],"x-scalar":"solo"}}}}}`
	rec := httptest.NewRecorder()
	status := applyResponseHead(rec, doc)
	if status != 201 {
		t.Fatalf("status = %d, want 201", status)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("content-type = %q", got)
	}
	if got := rec.Header().Get("X-Multi"); got != "b" {
		t.Errorf("x-multi = %q, want last-wins b", got)
	}
	if got := rec.Header().Get("X-Scalar"); got != "solo" {
		t.Errorf("x-scalar = %q", got)
	}
}
