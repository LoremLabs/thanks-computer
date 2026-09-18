package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/signedurl"
)

// denyTenant refuses one tenant and admits everyone else.
type denyTenant struct {
	admission.NopProvider
	slug string
}

func (d denyTenant) Decide(t string) admission.Decision {
	return admission.Decision{Admit: t != d.slug, Status: http.StatusForbidden}
}

func newSignDeps(t *testing.T) driveDeps {
	t.Helper()
	d := newDriveDeps(t, map[string]string{"pony.example.com": "acme"})
	signer, err := signedurl.New(bytes.Repeat([]byte{3}, 32), 1)
	if err != nil {
		t.Fatal(err)
	}
	d.signer, d.signBase = signer, "https://files.example.com"
	// The endpoint checks expiry against the wall clock, so the op must sign
	// against it too.
	d.now = time.Now
	return d
}

func signedGet(t *testing.T, h http.Handler, method, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	path := url[strings.Index(url, signedurl.PathPrefix):]
	req := httptest.NewRequest(method, "https://anything.example.net"+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	return res, body
}

func TestDriveSignAndFetch(t *testing.T) {
	d := newSignDeps(t)
	callDrive(t, driveCollection, d, "acme", `{"name":"docs"}`)
	out := callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"report.pdf","value":"%PDF-1.7 the whole document","encoding":"utf8","content_type":"application/pdf"}`)
	if gjson.Get(out, "_drive.error").Exists() {
		t.Fatalf("put = %s", out)
	}
	want := []byte("%PDF-1.7 the whole document")
	sum := sha256.Sum256(want)
	sha := hex.EncodeToString(sum[:])

	out = callDrive(t, driveSign, d, "acme", `{"collection":"docs","path":"report.pdf","into":"_sig"}`)
	if gjson.Get(out, "_sig.error").Exists() {
		t.Fatalf("sign = %s", out)
	}
	url := gjson.Get(out, "_sig.url").String()
	if !strings.HasPrefix(url, "https://files.example.com/_txc/signed/v1.") {
		t.Fatalf("url = %q", url)
	}
	if gjson.Get(out, "_sig.sha256").String() != sha || gjson.Get(out, "_sig.size").Int() != int64(len(want)) ||
		gjson.Get(out, "_sig.content_type").String() != "application/pdf" || gjson.Get(out, "_sig.name").String() != "report.pdf" ||
		gjson.Get(out, "_sig.ttl").Int() != 600 {
		t.Errorf("sign facts = %s", out)
	}
	exp, err := time.Parse(time.RFC3339, gjson.Get(out, "_sig.expires_at").String())
	if err != nil || time.Until(exp) < 9*time.Minute || time.Until(exp) > 10*time.Minute {
		t.Errorf("expires_at = %q (%v)", gjson.Get(out, "_sig.expires_at").String(), err)
	}

	h := signedHandler(d.store, d.signer, admission.NopProvider{}, nil)

	res, body := signedGet(t, h, http.MethodGet, url, nil)
	if res.StatusCode != 200 || !bytes.Equal(body, want) {
		t.Fatalf("GET = %d %q", res.StatusCode, body)
	}
	// Inert wherever it lands: never a page on somebody's origin.
	for k, v := range map[string]string{
		"Content-Type":            "application/pdf",
		"Content-Disposition":     "attachment",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox; default-src 'none'",
		"Cache-Control":           "private, no-store",
		"Etag":                    `"` + sha + `"`,
	} {
		if got := res.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	res, body = signedGet(t, h, http.MethodHead, url, nil)
	if res.StatusCode != 200 || len(body) != 0 || res.ContentLength != int64(len(want)) {
		t.Errorf("HEAD = %d, %d body bytes, Content-Length %d", res.StatusCode, len(body), res.ContentLength)
	}
	// A fetcher may resume: ranges work.
	res, body = signedGet(t, h, http.MethodGet, url, map[string]string{"Range": "bytes=0-7"})
	if res.StatusCode != http.StatusPartialContent || string(body) != "%PDF-1.7" {
		t.Errorf("range = %d %q", res.StatusCode, body)
	}

	// By resource id, with a ttl.
	id := gjson.Get(out, "_sig.resource_id").String()
	out = callDrive(t, driveSign, d, "acme", `{"collection":"docs","resource_id":"`+id+`","ttl":30}`)
	if gjson.Get(out, "_drive.error").Exists() || gjson.Get(out, "_drive.ttl").Int() != 30 {
		t.Errorf("sign by id = %s", out)
	}
}

func TestDriveSignRefusals(t *testing.T) {
	d := newSignDeps(t)
	callDrive(t, driveCollection, d, "acme", `{"name":"docs"}`)
	callDrive(t, driveCollection, d, "other", `{"name":"private"}`)
	callDrive(t, driveMkdir, d, "acme", `{"collection":"docs","path":"folder"}`)
	callDrive(t, drivePut, d, "other", `{"collection":"private","path":"secret.txt","value":"x","encoding":"utf8"}`)

	for name, tc := range map[string]struct{ tenant, meta, code string }{
		"missing":          {"acme", `{"collection":"docs","path":"nope.pdf"}`, "txco_drive_not_found"},
		"directory":        {"acme", `{"collection":"docs","path":"folder"}`, "txco_drive_is_directory"},
		"no address":       {"acme", `{"collection":"docs"}`, "txco_drive_invalid_arg"},
		"other's tree":     {"acme", `{"collection":"private","path":"secret.txt"}`, "txco_drive_not_found"},
		"ttl too long":     {"acme", `{"collection":"docs","path":"folder","ttl":3601}`, "txco_drive_invalid_arg"},
		"ttl zero":         {"acme", `{"collection":"docs","path":"folder","ttl":0}`, "txco_drive_invalid_arg"},
		"ttl not a number": {"acme", `{"collection":"docs","path":"folder","ttl":"soon"}`, "txco_drive_invalid_arg"},
		"no tenant":        {"", `{"collection":"docs","path":"folder"}`, "txco_drive_no_tenant"},
	} {
		if got := gjson.Get(callDrive(t, driveSign, d, tc.tenant, tc.meta), "_drive.error.code").String(); got != tc.code {
			t.Errorf("%s: code = %q, want %q", name, got, tc.code)
		}
	}

	// No master key on the node ⇒ no signer ⇒ a named refusal, not a URL
	// nobody can redeem.
	d.signer = nil
	callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"a.txt","value":"a","encoding":"utf8"}`)
	if got := gjson.Get(callDrive(t, driveSign, d, "acme", `{"collection":"docs","path":"a.txt"}`), "_drive.error.code").String(); got != "txco_drive_sign_unavailable" {
		t.Errorf("no signer: code = %q", got)
	}
}

func TestSignedHandlerRefusals(t *testing.T) {
	d := newSignDeps(t)
	callDrive(t, driveCollection, d, "acme", `{"name":"docs"}`)
	callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"a.txt","value":"first","encoding":"utf8"}`)
	sign := func(meta string) string {
		t.Helper()
		out := callDrive(t, driveSign, d, "acme", meta)
		if gjson.Get(out, "_drive.error").Exists() {
			t.Fatalf("sign = %s", out)
		}
		return gjson.Get(out, "_drive.url").String()
	}
	url := sign(`{"collection":"docs","path":"a.txt"}`)
	h := signedHandler(d.store, d.signer, admission.NopProvider{}, nil)

	// Everything a guesser could try is the SAME answer.
	var first []byte
	for name, u := range map[string]string{
		"no token": "https://x/_txc/signed/",
		"garbage":  "https://x/_txc/signed/v1.AAAA.BBBB",
		"tampered": url[:len(url)-4] + "AAAA",
		"other key": func() string {
			o := d
			s, _ := signedurl.New(bytes.Repeat([]byte{4}, 32), 1)
			o.signer = s
			return gjson.Get(callDrive(t, driveSign, o, "acme", `{"collection":"docs","path":"a.txt"}`), "_drive.url").String()
		}(),
		"sub-path": url + "/extra",
		"expired": func() string {
			o := d
			o.now = func() time.Time { return time.Now().Add(-time.Hour) }
			return gjson.Get(callDrive(t, driveSign, o, "acme", `{"collection":"docs","path":"a.txt"}`), "_drive.url").String()
		}(),
	} {
		res, body := signedGet(t, h, http.MethodGet, u, nil)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, res.StatusCode)
		}
		if first == nil {
			first = body
		} else if !bytes.Equal(first, body) {
			t.Errorf("%s: body %q differs from %q — the refusals must be indistinguishable", name, body, first)
		}
	}

	// The file is rewritten after signing: the token pinned the old bytes and
	// must never read the new ones.
	callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"a.txt","value":"second, and private","encoding":"utf8"}`)
	res, body := signedGet(t, h, http.MethodGet, url, nil)
	if res.StatusCode != http.StatusGone || bytes.Contains(body, []byte("second")) {
		t.Errorf("after rewrite: %d %q, want 410 and none of the new bytes", res.StatusCode, body)
	}
	// Put the old bytes back and the same URL reads again: it is the CONTENT
	// that is pinned.
	callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"a.txt","value":"first","encoding":"utf8"}`)
	if res, body := signedGet(t, h, http.MethodGet, url, nil); res.StatusCode != 200 || string(body) != "first" {
		t.Errorf("content restored: %d %q", res.StatusCode, body)
	}

	// A rename keeps the resource id, so the URL survives it…
	if out := callDrive(t, driveMove, d, "acme", `{"collection":"docs","path":"a.txt","to":"b.txt"}`); gjson.Get(out, "_drive.error").Exists() {
		t.Fatalf("move = %s", out)
	}
	if res, _ := signedGet(t, h, http.MethodGet, url, nil); res.StatusCode != 200 {
		t.Errorf("after rename: %d, want 200", res.StatusCode)
	}
	// …and a delete ends it, as a plain 404.
	if out := callDrive(t, driveDelete, d, "acme", `{"collection":"docs","path":"b.txt"}`); gjson.Get(out, "_drive.error").Exists() {
		t.Fatalf("delete = %s", out)
	}
	if res, _ := signedGet(t, h, http.MethodGet, url, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("after delete: %d, want 404", res.StatusCode)
	}

	// A suspended tenant's URLs stop working, indistinguishably.
	callDrive(t, drivePut, d, "acme", `{"collection":"docs","path":"c.txt","value":"c","encoding":"utf8"}`)
	url = sign(`{"collection":"docs","path":"c.txt"}`)
	denied := signedHandler(d.store, d.signer, denyTenant{slug: "acme"}, nil)
	if res, body := signedGet(t, denied, http.MethodGet, url, nil); res.StatusCode != http.StatusNotFound || !bytes.Equal(body, first) {
		t.Errorf("suspended tenant: %d %q", res.StatusCode, body)
	}

	// A node that cannot serve (no store / no signer) is a 404, not a panic.
	if res, _ := signedGet(t, signedHandler(nil, nil, nil, nil), http.MethodGet, url, nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("unwired handler: %d", res.StatusCode)
	}
}

func TestSignedURLBase(t *testing.T) {
	for name, tc := range map[string]struct {
		conf config.Config
		want string
	}{
		"explicit":        {config.Config{SignedURLBase: "https://files.example.com/", ContinuationCallbackBaseURL: "https://cb.example.com"}, "https://files.example.com"},
		"callback base":   {config.Config{ContinuationCallbackBaseURL: "https://cb.example.com/"}, "https://cb.example.com"},
		"port only":       {config.Config{WebAddr: ":8080", Fqdn: "node.example.com"}, "http://node.example.com:8080"},
		"unspecified":     {config.Config{WebAddr: "0.0.0.0:8080", Fqdn: "node.example.com"}, "http://node.example.com:8080"},
		"loopback listen": {config.Config{WebAddr: "127.0.0.1:18080", Fqdn: "node.example.com"}, "http://127.0.0.1:18080"},
		"nothing known":   {config.Config{WebAddr: ":8080"}, "http://localhost:8080"},
	} {
		if got := signedURLBase(tc.conf); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
