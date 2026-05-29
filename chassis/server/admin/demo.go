package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/op"
	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/compute/storeresolver"
	"github.com/loremlabs/thanks-computer/chassis/demo"
)

// handleDemoCurriculum returns the demo walkthrough curriculum
// (tracks, steps, ops, request shape) the admin-ui's #demo route
// uses to render the walkthrough. Single source of truth — the same
// data feeds `txco demo`'s pre-seed (chassis/cli/demo → demo.Seed).
func (c *Controller) handleDemoCurriculum(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, demo.Get())
}

// demoFireRequest is the wire shape the demo UI POSTs to
// /v1/demo/fire: a synthetic HTTP request to replay against this
// chassis's own web inlet.
type demoFireRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// demoFireResponse mirrors what the web inlet returned, plus the
// X-Request-ID the UI uses to fetch the execution trace.
type demoFireResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	Rid     string            `json:"rid"`
}

// demoInfoResponse tells the demo UI where the web inlet lives,
// so a "copy as curl" can target the data plane (a different port than
// the admin server that serves the /demo/ UI).
type demoInfoResponse struct {
	WebAddr string `json:"web_addr"`
	WebPort string `json:"web_port"`
}

// handleDemoInfo reports the web inlet's listen port. The browser pairs
// it with its own hostname to build a curl that actually reaches the
// data plane (the /demo/ UI is served by the admin server on a
// different port).
func (c *Controller) handleDemoInfo(w http.ResponseWriter, r *http.Request) {
	webAddr := c.pu.Conf.WebAddr
	if webAddr == "" {
		webAddr = ":8080"
	}
	port := webAddr
	if i := strings.LastIndexByte(webAddr, ':'); i >= 0 {
		port = webAddr[i+1:]
	}
	writeJSON(w, http.StatusOK, demoInfoResponse{WebAddr: webAddr, WebPort: port})
}

// handleDemoFire is the demo's execution hop. The /demo/ UI is
// served by THIS (admin) server, but txcl runs on the web inlet — a
// different listener/port, so the browser can't fire it directly
// (cross-origin, and the inlet sets no CORS headers). This proxies the
// request to the loopback web inlet and returns its status/headers/body
// plus the X-Request-ID, which the UI then uses to fetch the trace via
// the existing /traces endpoints.
//
// Loopback-only by construction: it dials 127.0.0.1 + the configured
// web addr and adds no new externally reachable surface.
func (c *Controller) handleDemoFire(w http.ResponseWriter, r *http.Request) {
	var req demoFireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad request body", map[string]any{"err": err.Error()})
		return
	}

	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	path := req.Path
	if path == "" || !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Derive the loopback web-inlet base from the configured web addr
	// (e.g. ":8080" → "http://127.0.0.1:8080").
	webAddr := c.pu.Conf.WebAddr
	if webAddr == "" {
		webAddr = ":8080"
	}
	if strings.HasPrefix(webAddr, ":") {
		webAddr = "127.0.0.1" + webAddr
	}
	target := "http://" + webAddr + path

	var bodyReader io.Reader
	if req.Body != "" {
		bodyReader = strings.NewReader(req.Body)
	}
	outReq, err := http.NewRequestWithContext(r.Context(), method, target, bodyReader)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "build request failed", map[string]any{"err": err.Error()})
		return
	}
	for k, v := range req.Headers {
		// Host can't be set via Header.Set (net/http reads req.Host);
		// honor it explicitly so the UI can route the synthetic request
		// to the bound demo hostname (→ scratch stack).
		if strings.EqualFold(k, "Host") {
			outReq.Host = v
			continue
		}
		outReq.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(outReq)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "web inlet request failed", map[string]any{"err": err.Error()})
		return
	}
	defer resp.Body.Close()
	// Cap the proxied body so a runaway rule can't balloon the response.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB

	headers := make(map[string]string, len(resp.Header))
	for k := range resp.Header {
		headers[k] = resp.Header.Get(k)
	}

	writeJSON(w, http.StatusOK, demoFireResponse{
		Status:  resp.StatusCode,
		Headers: headers,
		Body:    string(respBody),
		Rid:     resp.Header.Get("X-Request-ID"),
	})
}

// demoBuildOpRequest is the wire shape the demo UI POSTs to
// /v1/demo/op/build: a single JavaScript (or TypeScript) compute-op
// source, to be bundled + compiled + stored as a content-addressed
// wasm artifact the runtime can resolve.
type demoBuildOpRequest struct {
	Source string `json:"source"`
	Lang   string `json:"lang,omitempty"` // "js" (default), "ts", "mjs"
}

// demoBuildOpResponse is what the demo client gets back: a
// `compute://sha256/<digest>` ref to splice into the op's txcl in place
// of `op://<name>`, plus the digest/engine/byte size for debugging.
type demoBuildOpResponse struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
	Engine string `json:"engine"`
	Bytes  int    `json:"bytes"`
}

// handleDemoBuildOp builds a single compute-op source (JS/TS) into a
// wasm artifact and stores it in this chassis's artifact store so the
// runtime resolver can load it on EXEC. It's the demo's analog of
// the CLI's `txco apply` compute path: reuses the same bundler + javy
// (`chassis/cli/op.BuildFile`) and the same astore.Put `handlePutCompute`
// writes through.
//
// Demo-only, loopback-only by construction (no new externally reachable
// surface; uses the same protected subrouter as /v1/demo/fire).
//
// The cache dir is stable across requests (`<tmp>/txco-demo-compute`)
// so identical source skips javy on re-Run — important because each
// click of the demo's Run button calls this for every op with JS.
//
// Errors:
//   - astore missing → 503 compute_store_unavailable (matches handlePutCompute)
//   - body parse / bad lang → 400
//   - empty source → 400 empty_source
//   - bundle or javy compile error → 422 compile_error (detail is the cleaned
//     toolchain message)
//   - javy not on PATH → 422 compile_unavailable (the one case worth its own
//     code so the UI can surface an install hint cleanly)
//   - store_put failure → 500 store_put
func (c *Controller) handleDemoBuildOp(w http.ResponseWriter, r *http.Request) {
	if c.astore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "compute_store_unavailable", nil)
		return
	}

	var req demoBuildOpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", map[string]any{"err": err.Error()})
		return
	}
	if strings.TrimSpace(req.Source) == "" {
		writeJSONError(w, http.StatusBadRequest, "empty_source", nil)
		return
	}
	lang := strings.ToLower(strings.TrimSpace(req.Lang))
	if lang == "" {
		lang = "js"
	}
	var ext string
	switch lang {
	case "js":
		ext = ".js"
	case "ts":
		ext = ".ts"
	case "mjs":
		ext = ".mjs"
	default:
		writeJSONError(w, http.StatusBadRequest, "unsupported_lang",
			map[string]any{"lang": lang, "supported": "js, ts, mjs"})
		return
	}

	// Stable cache dir so repeated Runs of the same source skip javy
	// (BuildFile keys its wasm cache by bundled-source hash under
	// <workspaceRoot>/.txco/compute/). The entry FILE is per-request and
	// cleaned up; only the wasm cache survives, keyed by content.
	workspaceRoot := filepath.Join(os.TempDir(), "txco-demo-compute")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "workspace", map[string]any{"err": err.Error()})
		return
	}
	entry, err := os.CreateTemp(workspaceRoot, "entry-*"+ext)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "tempfile", map[string]any{"err": err.Error()})
		return
	}
	entryPath := entry.Name()
	defer os.Remove(entryPath)
	if _, werr := entry.Write([]byte(req.Source)); werr != nil {
		entry.Close()
		writeJSONError(w, http.StatusInternalServerError, "write_entry", map[string]any{"err": werr.Error()})
		return
	}
	entry.Close()

	built, err := op.BuildFile(entryPath, workspaceRoot)
	if err != nil {
		code := "compile_error"
		// dispatch.go formats this exact phrase when exec.LookPath("javy")
		// returns ErrNotFound — surface it distinctly so the UI can show
		// the install hint instead of a generic compile message.
		if strings.Contains(err.Error(), "javy not found on PATH") {
			code = "compile_unavailable"
		}
		writeJSONError(w, http.StatusUnprocessableEntity, code,
			map[string]any{"detail": cleanDemoBuildErr(err.Error())})
		return
	}

	ref := compute.Ref{Alg: built.Alg, Digest: built.Digest}
	manifest, _ := json.Marshal(storeresolver.Manifest{
		Engine: built.Engine, Alg: built.Alg, Digest: built.Digest,
	})
	if perr := c.astore.Put(r.Context(), ref.StoreRef(), built.Wasm, manifest); perr != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_put", map[string]any{"err": perr.Error()})
		return
	}

	writeJSON(w, http.StatusOK, demoBuildOpResponse{
		Ref: built.Ref, Digest: built.Digest, Engine: built.Engine, Bytes: len(built.Wasm),
	})
}

// pathLineColRe matches a per-line "<…/path>:LINE:COL:<space><message>" tail
// — esbuild and javy both surface diagnostics in this shape. The path part
// is anchored to the slash before the basename, so a stray "10:20" in the
// message body can't false-positive (no slash, no match). The captured
// group is everything from LINE onward, which is what the demo UI
// actually wants to show.
var pathLineColRe = regexp.MustCompile(`(?:^|/)[^/:]+:(\d+:\d+:\s.+)$`)

// cleanDemoBuildErr trims chassis-side wrapper labels and temp-dir paths
// out of a bundle/compile error message so the demo's error banner
// reads as the human-meaningful bits ("LINE:COL: <message>"). The CLI's
// `txco apply` keeps the full err.Error() from op.BuildFile — those users
// often need the full path for grepping; the demo user is editing in
// a textarea where the path is just `<tmp>/txco-demo-compute/entry-XXXX.js`,
// which is noise.
//
// Untouched on a clean single-line message (e.g. "javy not found on PATH;
// install it …") so the install-hint compile_unavailable path still reads
// well. If cleaning would eat every line, we return the raw input — better
// to show noise than to swallow the diagnostic.
func cleanDemoBuildErr(raw string) string {
	out := make([]string, 0, 4)
	for _, ln := range strings.Split(raw, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		// fmt.Errorf wrappers from chassis/cli/op (bundle.go + dispatch.go):
		//   "bundle <basename>:"            (bundle errors)
		//   "compile error in <path>:"      (javy errors)
		// They describe what comes next; the next line carries the useful
		// content. Drop them.
		if strings.HasPrefix(ln, "bundle ") && strings.HasSuffix(ln, ":") {
			continue
		}
		if strings.HasPrefix(ln, "compile error in ") {
			continue
		}
		// "<path>:LINE:COL: <message>" → "LINE:COL: <message>".
		if m := pathLineColRe.FindStringSubmatch(ln); m != nil {
			out = append(out, m[1])
			continue
		}
		out = append(out, ln)
	}
	if len(out) == 0 {
		return raw
	}
	return strings.Join(out, "\n")
}
