package cloud

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

// callbackResult is what the loopback handler captures from the OAuth
// redirect.
type callbackResult struct {
	Code  string
	State string
	Err   *oauthError
}

// loopbackServer is a one-shot HTTP server that captures a single OAuth
// callback on a 127.0.0.1 port.
type loopbackServer struct {
	srv      *http.Server
	port     int
	resultCh chan callbackResult
	once     sync.Once
}

// startLoopbackServer binds the first free port from loopbackPorts on
// 127.0.0.1 (loopback only — never 0.0.0.0) and serves /callback. The
// returned server's Port()/RedirectURI() reflect the chosen port, which
// must match a registered redirect_uri. Caller must Close() it.
func startLoopbackServer() (*loopbackServer, error) {
	var ln net.Listener
	var port int
	for _, p := range loopbackPorts {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln, port = l, p
			break
		}
	}
	if ln == nil {
		return nil, fmt.Errorf("no free callback port in %d-%d (is another `txco login` running?)",
			loopbackPorts[0], loopbackPorts[len(loopbackPorts)-1])
	}

	ls := &loopbackServer{
		port:     port,
		resultCh: make(chan callbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", ls.handleCallback)
	ls.srv = &http.Server{Handler: mux}
	go func() { _ = ls.srv.Serve(ln) }()
	return ls, nil
}

// Port returns the bound loopback port.
func (ls *loopbackServer) Port() int { return ls.port }

// RedirectURI returns the exact redirect_uri for the chosen port. It MUST
// be one of the redirect_uris registered for the OAuth client.
func (ls *loopbackServer) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", ls.port)
}

func (ls *loopbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res := callbackResult{
		Code:  q.Get("code"),
		State: q.Get("state"),
	}
	if e := q.Get("error"); e != "" {
		res.Err = &oauthError{Code: e, Description: q.Get("error_description")}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if res.Err != nil || res.Code == "" {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(callbackHTML("Sign-in failed.",
			"Close this tab and return to your terminal.", true)))
	} else {
		_, _ = w.Write([]byte(callbackHTML("Signed in.",
			"Close this tab and return to your terminal.", false)))
	}
	ls.once.Do(func() { ls.resultCh <- res })
}

// Wait blocks for the single callback or for ctx cancellation/timeout.
func (ls *loopbackServer) Wait(ctx context.Context) (callbackResult, error) {
	select {
	case res := <-ls.resultCh:
		return res, nil
	case <-ctx.Done():
		return callbackResult{}, ctx.Err()
	}
}

// Close shuts the server down.
func (ls *loopbackServer) Close() {
	_ = ls.srv.Close()
}

// callbackHTML renders the loopback "you can close this tab" page, borrowing
// the continuation-ui brand treatment (mono font, neutral card, and the
// "thanks, cooomputer." wordmark whose three o's cycle the CMY triad).
func callbackHTML(status, sub string, isError bool) string {
	statusColor := "#404040" // neutral-700
	script := ""
	if isError {
		statusColor = "oklch(0.62 0.22 25)" // brand red
	} else {
		// Best-effort auto-close 2s after a successful sign-in. Browsers only
		// allow script to close script-opened windows, so this may be a no-op
		// (the visible "return to your terminal" text covers that case).
		script = `<script>setTimeout(function(){window.close();},2000);</script>`
	}
	return strings.NewReplacer(
		"__COLOR__", statusColor,
		"__STATUS__", status,
		"__SUB__", sub,
		"__SCRIPT__", script,
	).Replace(callbackPage)
}

// callbackPage is the static loopback page template (placeholders filled by
// callbackHTML). Brand tokens mirror continuation-ui/src/app.css.
const callbackPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<meta name="theme-color" content="#06b6d4">
<title>thanks, computer.</title>
<style>
  :root{
    --cyan: oklch(0.79 0.17 195);
    --magenta: oklch(0.65 0.27 330);
    --yellow: oklch(0.85 0.18 90);
    --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Monaco, Consolas, monospace;
  }
  html,body{height:100%;margin:0}
  body{font-family:var(--mono);background:#fafafa;color:#171717;display:flex;align-items:center;justify-content:center;min-height:100%;padding:1rem}
  .card{width:100%;max-width:28rem;background:#fff;border:1px solid #e5e5e5;border-radius:.5rem;padding:2.5rem;text-align:center;box-shadow:0 1px 2px rgba(0,0,0,.05)}
  .wordmark{font-size:1.5rem;font-weight:600;letter-spacing:-.01em}
  .status{margin-top:1.5rem;font-size:.875rem;color:__COLOR__}
  .sub{margin-top:.5rem;font-size:.75rem;color:#a3a3a3}
  @keyframes cmy{0%,100%{color:var(--cyan)}33%{color:var(--magenta)}66%{color:var(--yellow)}}
  .o{animation:cmy 2.4s steps(1,end) infinite}
  .o1{animation-delay:0s}.o2{animation-delay:-.8s}.o3{animation-delay:-1.6s}
  @media (prefers-reduced-motion:reduce){.o{animation:none}.o1{color:var(--cyan)}.o2{color:var(--magenta)}.o3{color:var(--yellow)}}
</style>
</head>
<body>
  <div class="card">
    <div class="wordmark">thanks, c<span class="o o1">o</span><span class="o o2">o</span><span class="o o3">o</span>mputer.</div>
    <p class="status">__STATUS__</p>
    <p class="sub">__SUB__</p>
  </div>
__SCRIPT__
</body>
</html>`
