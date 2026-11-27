package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/attached"
	"github.com/loremlabs/thanks-computer/chassis/attached/home"
	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// `txco attach <site>`: confirm in the browser, with no code to copy.
//
// The site names its confirm page at /.well-known/txco-attach, as
// {"page": "/attach"} (relative to where that answer came from, or whole).
// The terminal opens the page with an address on this machine to come back
// to and a random state. The page makes a code and sends the browser back
// with it; only a callback carrying the state is taken, and only once.
// Then the code is spent as a typed one is, without the question: the page
// asked it.
//
// A site that answers `"while_running": true` gets a share that ends when
// this command stops (home.Credential.WhileRunning).
//
// The fallback is the same page without the address to come back to: it
// shows the code, typed at the terminal's prompt. A typed code is asked
// about first, as with `txco attach <code>`.
//
// The site argument's query goes to the page as it is
// (`txco attach 'www.onepony.com?pony=sc.test'`), under the terminal's own
// machine, folder, redirect_uri and state, which always win.

const (
	// WellKnownAttach is where a site names its confirm page.
	WellKnownAttach = "/.well-known/txco-attach"
	// attachWait is how long the terminal waits for the page.
	attachWait = 15 * time.Minute
	// attachAnswerWait is how long the browser waits to hear how it went.
	attachAnswerWait = time.Minute
)

// For tests.
var (
	attachOpen = openAttachPage
	attachTTY  = auth.StdinIsTTY
)

// openAttachPage opens u in $BROWSER when it is set, else the system's
// browser; never over ssh, where it would open on the far computer's
// screen, if anywhere.
func openAttachPage(u string) error {
	if b := strings.Fields(os.Getenv("BROWSER")); len(b) > 0 {
		return exec.Command(b[0], append(b[1:], u)...).Start()
	}
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return errors.New("over ssh")
	}
	return openBrowser(u)
}

// isSite reports whether arg names a site rather than a code. A code is
// words and dashes, with its address after an @.
func isSite(arg string) bool {
	return !strings.Contains(arg, "@") && strings.ContainsAny(arg, ".:/")
}

// siteBase is the site arg names, scheme and host, and its query, for the
// page. A bare host is https; plain http only for this machine.
func siteBase(arg string) (*url.URL, url.Values, error) {
	s := strings.TrimSpace(arg)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, nil, fmt.Errorf("%q is not a site", arg)
	}
	if err := webOK(u); err != nil {
		return nil, nil, err
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, u.Query(), nil
}

func webOK(u *url.URL) error {
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLocalHost(u.Hostname()):
		return nil
	}
	return fmt.Errorf("%s: https only", u.Redacted())
}

func isLocalHost(h string) bool {
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// attachPage reads where site's confirm page is, and whether the site asks
// that a share end when this command stops (`"while_running": true`).
func attachPage(ctx context.Context, hc *http.Client, site *url.URL) (*url.URL, bool, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.String()+WellKnownAttach, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	notHere := fmt.Errorf("%s does not take txco attach", site.Host)
	if resp.StatusCode != http.StatusOK {
		return nil, false, notHere
	}
	var out struct {
		Page         string `json:"page"`
		WhileRunning bool   `json:"while_running"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if json.Unmarshal(raw, &out) != nil || out.Page == "" {
		return nil, false, notHere
	}
	ref, err := url.Parse(out.Page)
	if err != nil {
		return nil, false, notHere
	}
	// Relative to where the answer came from, after any redirect.
	page := resp.Request.URL.ResolveReference(ref)
	if err := webOK(page); err != nil {
		return nil, false, err
	}
	page.Fragment = ""
	return page, out.WhileRunning, nil
}

// withQuery is u with q added to its own query.
func withQuery(u *url.URL, q url.Values) string {
	c := *u
	have := c.Query()
	for k, v := range q {
		have[k] = v
	}
	c.RawQuery = have.Encode()
	return c.String()
}

func newAttachState() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func attachBrowser(h home.Home, arg string, yes bool, stdin io.Reader, stdout io.Writer) error {
	site, asked, err := siteBase(arg)
	if err != nil {
		return err
	}
	root, err := attachRoot()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, attachWait)
	defer cancel()

	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	page, whileRunning, err := attachPage(rctx, nil, site)
	rcancel()
	if err != nil {
		return err
	}
	state, err := newAttachState()
	if err != nil {
		return err
	}
	machine, _ := os.Hostname()
	about := url.Values{}
	for k, v := range asked {
		about[k] = v
	}
	about.Set("machine", machine)
	about.Set("folder", home.Pretty(root))

	// The browser on this machine comes back here.
	var (
		opened bool
		codes  chan attachCallback
	)
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		codes = make(chan attachCallback, 1)
		srv := &http.Server{Handler: attachCallbackHandler(state, page, codes), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)
		// Shutdown, not Close: the browser's redirect is written first.
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			srv.Shutdown(sctx)
		}()
		back := url.Values{}
		for k, v := range about {
			back[k] = v
		}
		back.Set("redirect_uri", fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port))
		back.Set("state", state)
		opened = attachOpen(withQuery(page, back)) == nil
	}
	fallback := withQuery(page, about)
	tty := attachTTY()
	if opened {
		fmt.Fprintln(stdout, "Confirm in your browser.")
		if tty {
			fmt.Fprintf(stdout, "Or open this and enter the code it shows:\n\n  %s\n\n", fallback)
		}
	} else if tty {
		fmt.Fprintf(stdout, "Open this and enter the code it shows:\n\n  %s\n\n", fallback)
	} else {
		return fmt.Errorf("no browser here; open %s on any computer and run txco attach <code> with the code it shows", fallback)
	}

	var typed <-chan string
	if tty {
		fmt.Fprint(stdout, "Code: ")
		typed = readLines(stdin)
	}
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(stdout)
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errors.New("timed out; not attached")
			}
			return errors.New("stopped; not attached")
		case cb := <-codes:
			fmt.Fprintln(stdout)
			if cb.err != "" {
				return errors.New("cancelled in the browser; not attached")
			}
			result := "failed"
			defer func() { cb.answer <- result }()
			code, base := attached.SplitCode(cb.code)
			if code == "" || base == "" {
				return errors.New("the page sent no code")
			}
			if u, err := url.Parse(base); err != nil || webOK(u) != nil {
				return fmt.Errorf("the page sent a code for %s: https only", base)
			}
			if err := spendCode(h, code, base, root, whileRunning, nil, stdout); err != nil {
				return err
			}
			result = "ok"
			return nil
		case line, ok := <-typed:
			if !ok {
				typed = nil
				continue
			}
			code, base := attached.SplitCode(line)
			if code == "" || base == "" {
				fmt.Fprint(stdout, "Not a code. Code: ")
				continue
			}
			var ask func() bool
			if !yes {
				ask = func() bool {
					fmt.Fprint(stdout, "Attach? [y/N] ")
					a, ok := <-typed
					return ok && isYes(a)
				}
			}
			return spendCode(h, code, base, root, whileRunning, ask, stdout)
		}
	}
}

// attachCallback is the browser's one redirect: a code, or a refusal, and
// where to say how it went.
type attachCallback struct {
	code, err string
	answer    chan<- string
}

// attachCallbackHandler takes the browser's redirect with this attach's
// state, once, and sends the browser back to the page with ?result=ok,
// failed or cancelled.
func attachCallbackHandler(state string, page *url.URL, out chan<- attachCallback) http.Handler {
	var mu sync.Mutex
	taken := false
	done := func(result string) string {
		return withQuery(page, url.Values{"result": {result}})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "Not this attach. Run txco attach again.", http.StatusBadRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mu.Lock()
		first := !taken
		taken = true
		mu.Unlock()
		if !first {
			http.Redirect(w, r, page.String(), http.StatusFound)
			return
		}
		if q.Get("error") != "" {
			out <- attachCallback{err: q.Get("error")}
			http.Redirect(w, r, done("cancelled"), http.StatusFound)
			return
		}
		answer := make(chan string, 1)
		out <- attachCallback{code: q.Get("code"), answer: answer}
		result := "failed"
		select {
		case result = <-answer:
		case <-time.After(attachAnswerWait):
		case <-r.Context().Done():
			return
		}
		http.Redirect(w, r, done(result), http.StatusFound)
	})
}

// readLines reads trimmed lines from r, the empty ones skipped; the channel
// closes at the end of r.
func readLines(r io.Reader) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if s := strings.TrimSpace(sc.Text()); s != "" {
				out <- s
			}
		}
	}()
	return out
}
