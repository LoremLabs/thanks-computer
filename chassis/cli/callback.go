package cli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// callbackTimeout bounds how long the CLI waits for a hosted-page flow (Stripe
// Checkout, OAuth) to redirect back to the local loopback server.
const callbackTimeout = 15 * time.Minute

// loopback is the CLI's local 127.0.0.1 callback server for a hosted-page flow:
// the page's return target redirects the browser here, which unblocks the
// waiting command. A random state nonce guards against a stray localhost hit
// completing the flow.
type loopback struct {
	srv      *http.Server
	url      string // http://127.0.0.1:<port>/callback
	state    string
	resultCh chan callbackResult
}

type callbackResult struct {
	status    string // "success" | "cancel" | …
	sessionID string
}

// startLoopback binds an ephemeral 127.0.0.1 port and serves /callback.
func startLoopback() (*loopback, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	state, err := randomToken()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	lb := &loopback{
		url:      fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port),
		state:    state,
		resultCh: make(chan callbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", lb.handle)
	lb.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = lb.srv.Serve(ln) }()
	return lb, nil
}

func (lb *loopback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("state") != lb.state {
		http.Error(w, "unexpected callback state", http.StatusBadRequest)
		return
	}
	status := q.Get("status")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, returnPageHTML(status))
	select {
	case lb.resultCh <- callbackResult{status: status, sessionID: q.Get("session_id")}:
	default: // already delivered; ignore duplicate/refresh hits
	}
}

// wait blocks until the browser redirects back, or the timeout elapses.
func (lb *loopback) wait(timeout time.Duration) (callbackResult, bool) {
	select {
	case res := <-lb.resultCh:
		return res, true
	case <-time.After(timeout):
		return callbackResult{}, false
	}
}

func (lb *loopback) close() { _ = lb.srv.Close() }

// awaitCallback blocks on the loopback after a hosted-page flow was opened, and
// renders the outcome. opened=false means the user declined to open the URL (or
// we couldn't), so nothing will call back — don't hang.
func awaitCallback(lb *loopback, opened bool, stdout, stderr io.Writer) int {
	if !opened {
		fmt.Fprintln(stderr, "Open the URL above to finish; not waiting for a result.")
		return 1
	}
	fmt.Fprintln(stderr, "\nWaiting for you to finish in the browser (Ctrl-C to stop)…")
	res, ok := lb.wait(callbackTimeout)
	if !ok {
		fmt.Fprintln(stderr, "Timed out waiting for the browser.")
		return 1
	}
	if res.status == "success" {
		fmt.Fprintln(stdout, "\n✓ Completed in the browser.")
		return 0
	}
	fmt.Fprintln(stdout, "\nCanceled.")
	return 1
}

func randomToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func returnPageHTML(status string) string {
	msg := "All set — your purchase is complete."
	if status != "success" {
		msg = "Checkout canceled."
	}
	return `<!doctype html><html><head><meta charset="utf-8"><title>txco</title>` +
		`<style>body{font-family:system-ui,-apple-system,sans-serif;background:#0b0b0f;color:#eaeaf0;` +
		`display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}` +
		`.card{text-align:center;padding:2rem}.m{font-size:1.25rem;margin-bottom:.5rem}` +
		`.s{color:#8a8a99}</style></head><body><div class="card">` +
		`<div class="m">` + msg + `</div>` +
		`<div class="s">You can close this window and return to your terminal.</div>` +
		`</div></body></html>`
}
