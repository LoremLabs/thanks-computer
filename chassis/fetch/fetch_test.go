package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/private"
)

// Public addresses the fake resolver hands out; the recording dialer
// connects whatever was approved to a local test server. The guard is the
// real "private" policy, so these tests check the address actually
// dialed, not the URL validator.
const (
	pubA = "93.184.215.14"
	pubB = "93.184.215.15"
)

type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]netip.Addr // host → successive answers (last repeats)
	calls   map[string]int
}

func (r *fakeResolver) set(host string, answers ...[]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.answers == nil {
		r.answers = map[string][][]netip.Addr{}
		r.calls = map[string]int{}
	}
	var out [][]netip.Addr
	for _, a := range answers {
		var addrs []netip.Addr
		for _, s := range a {
			addrs = append(addrs, netip.MustParseAddr(s))
		}
		out = append(out, addrs)
	}
	r.answers[host] = out
}

func (r *fakeResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.answers[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := r.calls[host]
	r.calls[host]++
	if i >= len(a) {
		i = len(a) - 1
	}
	return a[i], nil
}

// recorder is the Dial seam: it records every approved address and
// connects it to the test server.
type recorder struct {
	mu     sync.Mutex
	dials  []string
	target string
}

func (rc *recorder) dial(ctx context.Context, _ string, address string) (net.Conn, error) {
	rc.mu.Lock()
	rc.dials = append(rc.dials, address)
	target := rc.target
	rc.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", target)
}

func (rc *recorder) got() []string {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]string(nil), rc.dials...)
}

func privateGuard(t *testing.T) egress.Guard {
	t.Helper()
	g, err := egress.Open("private", egress.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type harness struct {
	res *fakeResolver
	rec *recorder
	f   *Fetcher
}

func newHarness(t *testing.T, srv *httptest.Server, mod func(*Options)) *harness {
	t.Helper()
	h := &harness{res: &fakeResolver{}, rec: &recorder{}}
	if srv != nil {
		h.rec.target = srv.Listener.Addr().String()
	}
	opts := Options{
		Guard:      privateGuard(t),
		MaxBytes:   1 << 16,
		Timeout:    2 * time.Second,
		MediaTypes: []string{"text/html"},
		Accept:     "text/html",
		Resolver:   h.res,
		Dial:       h.rec.dial,
	}
	if srv != nil && srv.TLS != nil {
		pool := x509.NewCertPool()
		pool.AddCert(srv.Certificate())
		opts.TLSConfig = &tls.Config{RootCAs: pool}
	}
	if mod != nil {
		mod(&opts)
	}
	f, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.f = f
	return h
}

func htmlServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, body, r.Host)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wantCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a *fetch.Error with code %s", err, code)
	}
	if fe.Code != code {
		t.Fatalf("code = %s (%s), want %s", fe.Code, fe.Message, code)
	}
	return fe
}

func get(h *harness, u string) (*Result, error) {
	return h.f.Get(context.Background(), Request{URL: u, UserAgent: "txco-test"})
}

func TestGetPublicHostKeepsItsName(t *testing.T) {
	srv := htmlServer(t, "<title>%s</title>")
	h := newHarness(t, srv, nil)
	h.res.set("example.test", []string{pubA})

	res, err := get(h, "http://example.test/page?q=1")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "<title>example.test</title>" {
		t.Fatalf("body = %q: the Host header must stay the URL's host", res.Body)
	}
	if res.Status != 200 || res.ContentType != "text/html; charset=utf-8" || res.FinalURL != "http://example.test/page?q=1" {
		t.Fatalf("result = %+v", res)
	}
	if got := h.rec.got(); len(got) != 1 || got[0] != pubA+":80" {
		t.Fatalf("dials = %v, want [%s:80]", got, pubA)
	}
}

func TestURLPolicy(t *testing.T) {
	h := newHarness(t, nil, nil)
	for _, u := range []string{
		"",
		"ftp://example.test/",
		"file:///etc/passwd",
		"data:text/html,<title>x</title>",
		"javascript:alert(1)",
		"http://user:pass@example.test/",
		"http://user@example.test/",
		"http://example.test:8080/",
		"https://example.test:22/",
		"http:///nohost",
		"http://exa mple.test/",
		"mailto:a@example.test",
		"http://" + strings.Repeat("a", 5000) + ".test/",
	} {
		_, err := get(h, u)
		wantCode(t, err, CodeInvalidURL)
	}
	if got := h.rec.got(); len(got) != 0 {
		t.Fatalf("dials = %v, want none", got)
	}
}

func TestPrivateDestinationsAreNeverDialed(t *testing.T) {
	srv := htmlServer(t, "x")
	h := newHarness(t, srv, nil)
	cases := map[string][]string{
		"loop.test":      {"127.0.0.1"},
		"loop6.test":     {"::1"},
		"ten.test":       {"10.1.2.3"},
		"rfc1918.test":   {"192.168.1.1"},
		"cgnat.test":     {"100.64.0.1"},
		"meta.test":      {"169.254.169.254"},
		"ula.test":       {"fd00::1"},
		"fly6pn.test":    {"fdaa:0:1::2"},
		"linklocal.test": {"fe80::1"},
		"mapped.test":    {"::ffff:10.0.0.1"},
		"mapped6.test":   {"::ffff:127.0.0.1"},
		"nat64.test":     {"64:ff9b::a00:1"},
		"sixto4.test":    {"2002:a00:1::"},
		"zero.test":      {"0.0.0.0"},
	}
	for host, addrs := range cases {
		h.res.set(host, addrs)
		_, err := get(h, "http://"+host+"/")
		wantCode(t, err, CodeDestinationDenied)
	}
	for _, lit := range []string{
		"http://127.0.0.1/", "http://[::1]/", "http://169.254.169.254/latest/meta-data/",
		"http://[::ffff:127.0.0.1]/", "http://[fd00::1]/", "http://10.0.0.1/",
	} {
		_, err := get(h, lit)
		wantCode(t, err, CodeDestinationDenied)
	}
	if got := h.rec.got(); len(got) != 0 {
		t.Fatalf("dials = %v, want none", got)
	}
}

// A host answering both private and public addresses is dialed only at
// the public one.
func TestMixedAnswerDialsOnlyApprovedAddresses(t *testing.T) {
	srv := htmlServer(t, "ok")
	h := newHarness(t, srv, nil)
	h.res.set("mixed.test", []string{"10.0.0.7", pubA, "127.0.0.1"})
	if _, err := get(h, "http://mixed.test/"); err != nil {
		t.Fatal(err)
	}
	if got := h.rec.got(); len(got) != 1 || got[0] != pubA+":80" {
		t.Fatalf("dials = %v, want only %s:80", got, pubA)
	}
}

// DNS rebinding: the second lookup of the same name answers private. The
// check runs on every dial's own answer, so the second fetch is refused.
func TestRebindingIsCheckedAtEveryDial(t *testing.T) {
	srv := htmlServer(t, "ok")
	h := newHarness(t, srv, nil)
	h.res.set("rebind.test", []string{pubA}, []string{"127.0.0.1"})
	if _, err := get(h, "http://rebind.test/"); err != nil {
		t.Fatal(err)
	}
	h.f.client.CloseIdleConnections()
	_, err := get(h, "http://rebind.test/")
	wantCode(t, err, CodeDestinationDenied)
	if got := h.rec.got(); len(got) != 1 {
		t.Fatalf("dials = %v, want just the first", got)
	}
}

// Numeric spellings of loopback either fail to resolve or resolve to an
// address the guard refuses; they are never dialed. The system resolver
// is used here on purpose. (0177.0.0.1 is not one of them: the resolver
// reads it as decimal, 177.0.0.1, a public address.)
func TestNumericHostSpellings(t *testing.T) {
	srv := htmlServer(t, "x")
	h := newHarness(t, srv, func(o *Options) { o.Resolver = nil })
	for _, u := range []string{
		"http://2130706433/", "http://017700000001/", "http://0x7f000001/",
		"http://0x7f.0.0.1/", "http://127.1/",
	} {
		_, err := h.f.Get(context.Background(), Request{URL: u})
		var fe *Error
		if !errors.As(err, &fe) || (fe.Code != CodeDestinationDenied && fe.Code != CodeDNSFailed && fe.Code != CodeInvalidURL) {
			t.Fatalf("%s: err = %v, want denied, dns_failed or invalid_url", u, err)
		}
	}
	if got := h.rec.got(); len(got) != 0 {
		t.Fatalf("dials = %v, want none", got)
	}
}

func TestDNSFailure(t *testing.T) {
	h := newHarness(t, nil, nil)
	_, err := get(h, "https://unfurl.invalid/some/page")
	wantCode(t, err, CodeDNSFailed)
}

// The production dial asks the guard about the socket's own address too.
func TestGuardedDialRefusesPrivateSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, err = guardedDial(privateGuard(t))(context.Background(), "tcp", ln.Addr().String())
	if !isDenied(err) {
		t.Fatalf("err = %v, want denied", err)
	}
}

func redirectServer(t *testing.T, to func(r *http.Request) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loc := to(r); loc != "" {
			http.Redirect(w, r, loc, http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>landed</title>")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRedirects(t *testing.T) {
	var target atomic.Value
	target.Store("")
	srv := redirectServer(t, func(r *http.Request) string {
		if r.Host == "start.test" {
			return target.Load().(string)
		}
		return ""
	})
	h := newHarness(t, srv, nil)
	h.res.set("start.test", []string{pubA})
	h.res.set("public.test", []string{pubB})
	h.res.set("internal.test", []string{"10.9.9.9"})

	target.Store("http://public.test/landed")
	res, err := get(h, "http://start.test/")
	if err != nil {
		t.Fatal(err)
	}
	if res.FinalURL != "http://public.test/landed" || res.Redirects != 1 {
		t.Fatalf("final = %s redirects = %d", res.FinalURL, res.Redirects)
	}

	target.Store("http://internal.test/admin")
	_, err = get(h, "http://start.test/")
	wantCode(t, err, CodeDestinationDenied)

	for _, bad := range []string{"file:///etc/passwd", "http://public.test:8080/", "ftp://public.test/", "http://u:p@public.test/"} {
		target.Store(bad)
		_, err = get(h, "http://start.test/")
		wantCode(t, err, CodeInvalidURL)
	}
	for _, d := range h.rec.got() {
		if !strings.HasPrefix(d, pubA) && !strings.HasPrefix(d, pubB) {
			t.Fatalf("dialed %s", d)
		}
	}
}

func TestRedirectLimit(t *testing.T) {
	// /n redirects to /n-1; /0 lands.
	srv := redirectServer(t, func(r *http.Request) string {
		var n int
		fmt.Sscanf(r.URL.Path, "/%d", &n)
		if n > 0 {
			return fmt.Sprintf("/%d", n-1)
		}
		return ""
	})
	h := newHarness(t, srv, nil)
	h.res.set("hops.test", []string{pubA})
	res, err := get(h, "http://hops.test/3")
	if err != nil {
		t.Fatal(err)
	}
	if res.Redirects != 3 {
		t.Fatalf("redirects = %d, want 3", res.Redirects)
	}
	_, err = get(h, "http://hops.test/4")
	wantCode(t, err, CodeRedirectLimit)
}

func TestProxyEnvironmentIsIgnored(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	srv := htmlServer(t, "ok")
	h := newHarness(t, srv, nil)
	h.res.set("example.test", []string{pubA})
	if _, err := get(h, "http://example.test/"); err != nil {
		t.Fatal(err)
	}
	if got := h.rec.got(); len(got) != 1 || got[0] != pubA+":80" {
		t.Fatalf("dials = %v: the proxy must not be used", got)
	}
}

func TestTLSVerifiesTheURLsHost(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<title>tls</title>")
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, nil)
	h.res.set("example.com", []string{pubA})
	h.res.set("other.test", []string{pubB})

	res, err := get(h, "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "<title>tls</title>" {
		t.Fatalf("body = %q", res.Body)
	}
	// The certificate does not name other.test: the handshake fails even
	// though the bytes reach the same server.
	_, err = get(h, "https://other.test/")
	wantCode(t, err, CodeConnectionFailed)
}

// Connections are pooled per host: a second host gets its own dial, a
// repeat reuses its host's connection.
func TestConnectionReuseStaysWithItsHost(t *testing.T) {
	srv := htmlServer(t, "ok")
	h := newHarness(t, srv, nil)
	h.res.set("a.test", []string{pubA})
	h.res.set("b.test", []string{pubB})
	for _, u := range []string{"http://a.test/", "http://b.test/", "http://a.test/", "http://b.test/"} {
		if _, err := get(h, u); err != nil {
			t.Fatal(err)
		}
	}
	got := h.rec.got()
	if len(got) != 2 || got[0] != pubA+":80" || got[1] != pubB+":80" {
		t.Fatalf("dials = %v, want one per host", got)
	}
}

func TestContentTypeRules(t *testing.T) {
	var ct atomic.Value
	var unset atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unset.Load() {
			w.Header()["Content-Type"] = nil
		} else {
			w.Header().Set("Content-Type", ct.Load().(string))
		}
		fmt.Fprint(w, "<html><title>x</title></html>")
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, nil)
	h.res.set("ct.test", []string{pubA})

	for _, ok := range []string{"text/html", "text/html; charset=utf-8", "TEXT/HTML; charset=Shift_JIS"} {
		ct.Store(ok)
		res, err := get(h, "http://ct.test/")
		if err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
		if res.ContentType != ok {
			t.Fatalf("content type = %q, want %q as sent", res.ContentType, ok)
		}
	}
	for _, bad := range []string{"application/xhtml+xml", "application/json", "text/plain", "image/png", "text/htmlx", ";;;"} {
		ct.Store(bad)
		_, err := get(h, "http://ct.test/")
		fe := wantCode(t, err, CodeUnsupportedType)
		if fe.ContentType != bad || fe.Status != 200 {
			t.Fatalf("error = %+v, want the content type and status carried", fe)
		}
	}
	unset.Store(true)
	_, err := get(h, "http://ct.test/")
	wantCode(t, err, CodeUnsupportedType)
}

func TestHTTPErrorCarriesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<title>missing</title>")
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, nil)
	h.res.set("gone.test", []string{pubA})
	_, err := get(h, "http://gone.test/")
	fe := wantCode(t, err, CodeHTTPError)
	if fe.Status != 404 || fe.ContentType != "text/html" {
		t.Fatalf("error = %+v", fe)
	}
}

func TestOversizeBodies(t *testing.T) {
	big := strings.Repeat("a", 2<<16)
	var chunked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunked := chunked.Load()
		w.Header().Set("Content-Type", "text/html")
		if !chunked {
			w.Header().Set("Content-Length", fmt.Sprint(len(big)))
		}
		for i := 0; i < len(big); i += 4096 {
			w.Write([]byte(big[i : i+4096]))
			if chunked {
				w.(http.Flusher).Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, nil) // MaxBytes 64 KiB
	h.res.set("big.test", []string{pubA})
	_, err := get(h, "http://big.test/")
	wantCode(t, err, CodeResponseTooLarge)
	chunked.Store(true)
	_, err = get(h, "http://big.test/")
	fe := wantCode(t, err, CodeResponseTooLarge)
	if fe.Bytes <= 1<<16 || fe.Bytes > 1<<16+1 {
		t.Fatalf("bytes read = %d, want the cap plus one", fe.Bytes)
	}

	// Exactly the cap fits.
	exact := strings.Repeat("b", 1<<16)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, exact)
	}))
	t.Cleanup(srv2.Close)
	h2 := newHarness(t, srv2, nil)
	h2.res.set("exact.test", []string{pubA})
	res, err := get(h2, "http://exact.test/")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Body) != 1<<16 || res.Bytes != 1<<16 {
		t.Fatalf("len = %d bytes = %d", len(res.Body), res.Bytes)
	}
}

func gzipped(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	zw.Close()
	return b.Bytes()
}

func TestCompressedBodies(t *testing.T) {
	type reply struct {
		enc  string
		body []byte
	}
	var cur atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("Accept-Encoding = %q, want identity", r.Header.Get("Accept-Encoding"))
		}
		rp := cur.Load().(reply)
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Encoding", rp.enc)
		w.Write(rp.body)
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, nil)
	h.res.set("z.test", []string{pubA})

	// A server that sends gzip anyway is decoded.
	cur.Store(reply{"gzip", gzipped(t, "<title>zipped</title>")})
	res, err := get(h, "http://z.test/")
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Body) != "<title>zipped</title>" {
		t.Fatalf("body = %q", res.Body)
	}
	// A bomb: tiny on the wire, over the cap decoded.
	cur.Store(reply{"gzip", gzipped(t, strings.Repeat("a", 10<<20))})
	_, err = get(h, "http://z.test/")
	wantCode(t, err, CodeResponseTooLarge)
	// Anything else is refused.
	cur.Store(reply{"br", []byte("not really brotli")})
	_, err = get(h, "http://z.test/")
	wantCode(t, err, CodeUnsupportedEncoding)
}

func TestSlowBodyHitsTheDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	h := newHarness(t, srv, func(o *Options) { o.Timeout = 200 * time.Millisecond })
	h.res.set("slow.test", []string{pubA})
	start := time.Now()
	_, err := get(h, "http://slow.test/")
	wantCode(t, err, CodeTimeout)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("took %s, want about the 200ms deadline", time.Since(start))
	}
}

func TestSlowHeadersHitTheDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	h := newHarness(t, srv, func(o *Options) { o.Timeout = 200 * time.Millisecond })
	h.res.set("slow.test", []string{pubA})
	_, err := get(h, "http://slow.test/")
	wantCode(t, err, CodeTimeout)
}

func TestCancellationMidBody(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<title>"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	h := newHarness(t, srv, nil)
	h.res.set("slow.test", []string{pubA})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := h.f.Get(ctx, Request{URL: "http://slow.test/"})
	wantCode(t, err, CodeTimeout)
}

func TestHeaderFloodFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 200; i++ {
			w.Header().Set(fmt.Sprintf("X-Flood-%d", i), strings.Repeat("f", 1024))
		}
		w.Header().Set("Content-Type", "text/html")
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, nil)
	h.res.set("flood.test", []string{pubA})
	_, err := get(h, "http://flood.test/")
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("err = %v, want a fetch error", err)
	}
}

func TestRequestHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/html")
	}))
	t.Cleanup(srv.Close)
	h := newHarness(t, srv, nil)
	h.res.set("hdr.test", []string{pubA})
	if _, err := get(h, "http://hdr.test/"); err != nil {
		t.Fatal(err)
	}
	if got.Get("User-Agent") != "txco-test" || got.Get("Accept") != "text/html" || got.Get("Cookie") != "" || got.Get("Authorization") != "" {
		t.Fatalf("headers = %v", got)
	}
}

func TestErrorMessagesNameNoAddress(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.res.set("ten.test", []string{"10.1.2.3"})
	for _, u := range []string{"http://ten.test/", "http://10.1.2.3/", "https://unfurl.invalid/"} {
		_, err := get(h, u)
		var fe *Error
		if !errors.As(err, &fe) {
			t.Fatalf("%s: %v", u, err)
		}
		if strings.Contains(fe.Message, "10.") || strings.Contains(fe.Message, "invalid") {
			t.Fatalf("%s: message %q names an address or host", u, fe.Message)
		}
	}
}
