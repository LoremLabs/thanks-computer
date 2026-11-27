package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/attached"
	"github.com/loremlabs/thanks-computer/chassis/attached/home"
)

func TestIsSite(t *testing.T) {
	for arg, want := range map[string]bool{
		"www.onepony.com":                             true,
		"onepony.com":                                 true,
		"https://www.onepony.com":                     true,
		"http://localhost:5273":                       true,
		"localhost:8080":                              true,
		"granite-pillow-otter@attach.thanks.computer": false,
		"granite-pillow-otter":                        false,
		"t-shirt-yo-yo":                               false,
		"list":                                        false,
	} {
		if got := isSite(arg); got != want {
			t.Errorf("isSite(%q) = %v, want %v", arg, got, want)
		}
	}
}

func TestSiteBase(t *testing.T) {
	for arg, want := range map[string]string{
		"www.onepony.com":                 "https://www.onepony.com",
		"https://www.onepony.com/a/b?c=d": "https://www.onepony.com",
		"http://localhost:5273":           "http://localhost:5273",
		"http://ranch.localhost:18980":    "http://ranch.localhost:18980",
		"http://127.0.0.1:9":              "http://127.0.0.1:9",
		"http://www.onepony.com":          "",
		"ftp://x.com":                     "",
	} {
		u, _, err := siteBase(arg)
		got := ""
		if err == nil {
			got = u.String()
		}
		if got != want {
			t.Errorf("siteBase(%q) = %q (%v), want %q", arg, got, err, want)
		}
	}
}

func TestSiteQuery(t *testing.T) {
	_, q, err := siteBase("www.onepony.com/x?pony=sc.test")
	if err != nil || q.Encode() != "pony=sc.test" {
		t.Errorf("query %q, %v", q.Encode(), err)
	}
}

// The site's query reaches the page; the terminal's own values win over it.
func TestAttachBrowserForwardsQuery(t *testing.T) {
	s := newSite(t)
	var opened url.Values
	open, sent := browser(t, url.Values{"code": {"granite-pillow-otter@" + s.URL}})
	h := useAttach(t, func(raw string) error {
		u, _ := url.Parse(raw)
		opened = u.Query()
		return open(raw)
	}, false)
	arg := s.URL + "?pony=sc.test&state=forged&redirect_uri=http://127.0.0.1:1/callback&machine=x&folder=/"
	done := make(chan error, 1)
	go func() { done <- attachBrowser(h, arg, false, strings.NewReader(""), &bytes.Buffer{}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("attachBrowser: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no hand-back reached the terminal; opened %v", opened)
	}
	<-sent
	if opened.Get("pony") != "sc.test" || opened.Get("from") != "terminal" {
		t.Errorf("the page lost the site's query or its own: %v", opened)
	}
	if opened.Get("state") == "forged" || opened.Get("machine") == "x" || opened.Get("folder") == "/" || strings.Contains(opened.Get("redirect_uri"), ":1/") {
		t.Errorf("the site's query overrode the terminal's own: %v", opened)
	}
	if len(opened["state"]) != 1 || len(opened["machine"]) != 1 {
		t.Errorf("a value twice: %v", opened)
	}
}

func TestOpenAttachPageOverSSH(t *testing.T) {
	t.Setenv("BROWSER", "")
	t.Setenv("SSH_CONNECTION", "10.0.0.1 22 10.0.0.2 22")
	if err := openAttachPage("http://127.0.0.1:1/"); err == nil {
		t.Error("opened a browser over ssh")
	}
}

func TestAttachPage(t *testing.T) {
	answer := func(page string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != WellKnownAttach {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"page": page})
		}))
	}
	read := func(srv *httptest.Server) (string, error) {
		u, _ := url.Parse(srv.URL)
		p, _, err := attachPage(t.Context(), nil, u)
		if err != nil {
			return "", err
		}
		return p.String(), nil
	}

	rel := answer("/attach")
	defer rel.Close()
	if got, err := read(rel); err != nil || got != rel.URL+"/attach" {
		t.Errorf("relative page: %q, %v", got, err)
	}
	abs := answer("https://www.onepony.com/attach#x")
	defer abs.Close()
	if got, err := read(abs); err != nil || got != "https://www.onepony.com/attach" {
		t.Errorf("whole page: %q, %v", got, err)
	}
	plain := answer("http://www.onepony.com/attach")
	defer plain.Close()
	if _, err := read(plain); err == nil {
		t.Error("a plain-http page elsewhere was taken")
	}
	none := answer("")
	defer none.Close()
	if _, err := read(none); err == nil || !strings.Contains(err.Error(), "does not take txco attach") {
		t.Errorf("no page: %v", err)
	}
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	if _, err := read(missing); err == nil {
		t.Error("a site with no answer was taken")
	}

	// onepony.com sends every request on to www: the page is relative to
	// where the answer came from.
	www := answer("/attach")
	defer www.Close()
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, www.URL+r.URL.Path, http.StatusPermanentRedirect)
	}))
	defer bare.Close()
	if got, err := read(bare); err != nil || got != www.URL+"/attach" {
		t.Errorf("after a redirect: %q, %v", got, err)
	}
}

// site is a fake site and chassis in one: its confirm page's address, the
// attach routes, and what was asked of them.
type site struct {
	*httptest.Server
	mu       sync.Mutex
	enrolled []attached.EnrollBody
	peeked   int
	refuse   bool
	// whileRunning is what the site's well-known says.
	whileRunning bool
}

func newSite(t *testing.T) *site {
	s := &site{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch r.URL.Path {
		case WellKnownAttach:
			_ = json.NewEncoder(w).Encode(map[string]any{"page": "/attach?from=terminal", "while_running": s.whileRunning})
		case attached.PathCode:
			s.peeked++
			_ = json.NewEncoder(w).Encode(attached.CodeResult{Label: "the pony x", ExpiresAt: time.Now().Add(time.Minute)})
		case attached.PathEnroll:
			if s.refuse {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"bad_code","message":"the code is not valid"}}`))
				return
			}
			var in attached.EnrollBody
			_ = json.NewDecoder(r.Body).Decode(&in)
			s.enrolled = append(s.enrolled, in)
			_ = json.NewEncoder(w).Encode(attached.EnrollResult{Credential: "atc-0123456789abcdef0123456789abcdef", Name: "computer/x", Label: "the pony x"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// browser stands in for the page: it reads what the terminal opened and
// comes back with q (the state filled in, unless q names one), and reports
// where it was sent next.
func browser(t *testing.T, q url.Values) (open func(string) error, sent <-chan string) {
	out := make(chan string, 1)
	return func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			t.Errorf("opened %q", raw)
			return err
		}
		p := u.Query()
		if p.Get("machine") == "" || p.Get("folder") == "" || p.Get("from") != "terminal" {
			t.Errorf("the page was not told the machine, the folder and its own query: %s", raw)
		}
		back, err := url.Parse(p.Get("redirect_uri"))
		if err != nil || back.Scheme != "http" || back.Hostname() != "127.0.0.1" || back.Path != "/callback" {
			t.Errorf("redirect_uri %q", p.Get("redirect_uri"))
		}
		if q.Get("state") == "" {
			q.Set("state", p.Get("state"))
		}
		go func() {
			hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := hc.Get(back.String() + "?" + q.Encode())
			if err != nil {
				out <- "error: " + err.Error()
				return
			}
			resp.Body.Close()
			out <- resp.Header.Get("Location")
		}()
		return nil
	}, out
}

func useAttach(t *testing.T, open func(string) error, tty bool) home.Home {
	t.Helper()
	oldOpen, oldTTY := attachOpen, attachTTY
	attachOpen, attachTTY = open, func() bool { return tty }
	t.Cleanup(func() { attachOpen, attachTTY = oldOpen, oldTTY })
	return home.Home{Dir: t.TempDir()}
}

func TestAttachBrowserHandBack(t *testing.T) {
	s := newSite(t)
	open, sent := browser(t, url.Values{"code": {"granite-pillow-otter@" + s.URL}})
	h := useAttach(t, open, false)
	var out bytes.Buffer
	if err := attachBrowser(h, s.URL, false, strings.NewReader(""), &out); err != nil {
		t.Fatalf("attachBrowser: %v\n%s", err, out.String())
	}
	if got, want := <-sent, s.URL+"/attach?from=terminal&result=ok"; got != want {
		t.Errorf("browser sent to %q, want %q", got, want)
	}
	creds, err := h.Load()
	if err != nil || len(creds) != 1 || creds[0].Address != s.URL || creds[0].Name != "computer/x" {
		t.Fatalf("credentials: %+v, %v", creds, err)
	}
	if creds[0].WhileRunning || h.Holds(creds[0].ID) {
		t.Error("the site did not ask for a while-running share")
	}
	if s.peeked != 0 {
		t.Error("the page's code was asked about again")
	}
	if len(s.enrolled) != 1 || s.enrolled[0].Code != "granite-pillow-otter" {
		t.Errorf("enrolled %+v", s.enrolled)
	}
	if strings.Contains(out.String(), "Code:") {
		t.Errorf("not a terminal, yet asked for a code:\n%s", out.String())
	}
}

func TestAttachBrowserOtherState(t *testing.T) {
	s := newSite(t)
	open, sent := browser(t, url.Values{"code": {"granite-pillow-otter@" + s.URL}, "state": {"forged"}})
	opened := make(chan string, 1)
	h := useAttach(t, func(raw string) error { opened <- raw; return open(raw) }, false)
	done := make(chan error, 1)
	go func() { done <- attachBrowser(h, s.URL, false, strings.NewReader(""), &bytes.Buffer{}) }()
	raw := <-opened
	if got := <-sent; got != "" {
		t.Errorf("a forged state was sent on to %q", got)
	}
	select {
	case err := <-done:
		t.Fatalf("attachBrowser ended on a forged state: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if len(s.enrolled) != 0 {
		t.Error("a forged state enrolled")
	}
	// The real one is still taken afterwards.
	u, _ := url.Parse(raw)
	q := u.Query()
	resp, err := http.Get(q.Get("redirect_uri") + "?" + url.Values{"code": {"granite-pillow-otter@" + s.URL}, "state": {q.Get("state")}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-done; err != nil {
		t.Errorf("the real state after a forged one: %v", err)
	}
}

func TestAttachBrowserCancelled(t *testing.T) {
	s := newSite(t)
	open, sent := browser(t, url.Values{"error": {"access_denied"}})
	h := useAttach(t, open, false)
	err := attachBrowser(h, s.URL, false, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v", err)
	}
	if got := <-sent; !strings.HasSuffix(got, "result=cancelled") {
		t.Errorf("browser sent to %q", got)
	}
}

func TestAttachBrowserRefused(t *testing.T) {
	s := newSite(t)
	s.refuse = true
	open, sent := browser(t, url.Values{"code": {"granite-pillow-otter@" + s.URL}})
	h := useAttach(t, open, false)
	if err := attachBrowser(h, s.URL, false, strings.NewReader(""), &bytes.Buffer{}); err == nil {
		t.Error("a refused code attached")
	}
	if got := <-sent; !strings.HasSuffix(got, "result=failed") {
		t.Errorf("browser sent to %q", got)
	}
}

func TestAttachBrowserPlainCode(t *testing.T) {
	s := newSite(t)
	open, sent := browser(t, url.Values{"code": {"granite-pillow-otter@http://example.com"}})
	h := useAttach(t, open, false)
	if err := attachBrowser(h, s.URL, false, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "https only") {
		t.Errorf("a plain-http code elsewhere: %v", err)
	}
	if got := <-sent; !strings.HasSuffix(got, "result=failed") {
		t.Errorf("browser sent to %q", got)
	}
}

func TestAttachBrowserTyped(t *testing.T) {
	s := newSite(t)
	h := useAttach(t, func(string) error { return errNoBrowser }, true)
	var out bytes.Buffer
	in := strings.NewReader("www.onepony.com\ngranite-pillow-otter@" + s.URL + "\ny\n")
	if err := attachBrowser(h, s.URL, false, in, &out); err != nil {
		t.Fatalf("attachBrowser: %v\n%s", err, out.String())
	}
	for _, want := range []string{"Open this and enter the code it shows:", s.URL + "/attach?", "Not a code.", "the pony x", "Attach? [y/N]", "Enrolled."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "redirect_uri") {
		t.Errorf("the address to type from shows the terminal's own:\n%s", out.String())
	}
	if s.peeked != 1 || len(s.enrolled) != 1 {
		t.Errorf("peeked %d, enrolled %d", s.peeked, len(s.enrolled))
	}
}

func TestAttachBrowserTypedNo(t *testing.T) {
	s := newSite(t)
	h := useAttach(t, func(string) error { return errNoBrowser }, true)
	in := strings.NewReader("granite-pillow-otter@" + s.URL + "\nn\n")
	if err := attachBrowser(h, s.URL, false, in, &bytes.Buffer{}); err == nil {
		t.Error("attached on a no")
	}
	if len(s.enrolled) != 0 {
		t.Error("enrolled on a no")
	}
}

func TestAttachBrowserNowhere(t *testing.T) {
	s := newSite(t)
	h := useAttach(t, func(string) error { return errNoBrowser }, false)
	err := attachBrowser(h, s.URL, false, strings.NewReader(""), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "txco attach <code>") {
		t.Errorf("err = %v", err)
	}
}

var errNoBrowser = errors.New("no browser")

// A site that asks for it gets a share that ends with this command: its
// credential says so, and this process holds its lock from the start.
func TestAttachBrowserWhileRunning(t *testing.T) {
	s := newSite(t)
	s.whileRunning = true
	open, sent := browser(t, url.Values{"code": {"granite-pillow-otter@" + s.URL}})
	h := useAttach(t, open, false)
	var out bytes.Buffer
	if err := attachBrowser(h, s.URL, false, strings.NewReader(""), &out); err != nil {
		t.Fatalf("attachBrowser: %v\n%s", err, out.String())
	}
	<-sent
	creds, err := h.Load()
	if err != nil || len(creds) != 1 {
		t.Fatalf("credentials: %+v, %v", creds, err)
	}
	t.Cleanup(func() { h.Release(creds[0].ID) })
	if !creds[0].WhileRunning || !h.Holds(creds[0].ID) {
		t.Errorf("while running %v, held %v", creds[0].WhileRunning, h.Holds(creds[0].ID))
	}
}
