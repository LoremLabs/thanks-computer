package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	devpkg "github.com/loremlabs/thanks-computer/chassis/cli/dev"
	"github.com/loremlabs/thanks-computer/chassis/cli/oprefs"
	"github.com/loremlabs/thanks-computer/chassis/server/static"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/chassis/webabi"
)

// runWeb dispatches `txco web <command>`.
func runWeb(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printWebUsage(stderr)
		return 2
	}
	switch args[0] {
	case "check":
		return runWebCheck(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printWebUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "web: unknown command %q\n\n", args[0])
		printWebUsage(stderr)
		return 2
	}
}

func printWebUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: txco web <command>

Web ABI builds (a producer's output: txco-web.json, public/, server/, ops/).

Commands:
  check <abi-dir | stack>   Install a build on a scratch chassis and probe it
`)
}

// startChassisFn is startChassis, replaceable in tests.
var startChassisFn = startChassis

// webCheckReport is the result of `txco web check`, and its --json form.
type webCheckReport struct {
	ABIDir   string         `json:"abi_dir"`
	Stack    string         `json:"stack"`
	OK       bool           `json:"ok"`
	Manifest webCheckMan    `json:"manifest"`
	Problems []string       `json:"problems,omitempty"` // offline and install failures
	Warnings []string       `json:"warnings,omitempty"`
	Install  *webCheckInst  `json:"install,omitempty"`
	Probes   []probeResult  `json:"probes,omitempty"`
	Summary  map[string]int `json:"summary"`
}

type webCheckMan struct {
	ABI         int      `json:"abi"`
	ServerEntry string   `json:"server_entry,omitempty"`
	Immutable   []string `json:"immutable,omitempty"`
}

type webCheckInst struct {
	Version int64 `json:"version"`
	Files   int   `json:"files"`
	Ops     int   `json:"ops"`
	Markers int   `json:"markers"`
}

// probeResult is one probe's outcome: pass, warn, fail or skip.
type probeResult struct {
	Name         string `json:"name"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	Status       int    `json:"status,omitempty"`
	ContentType  string `json:"content_type,omitempty"`
	CacheControl string `json:"cache_control,omitempty"`
	ETag         string `json:"etag,omitempty"`
	Result       string `json:"result"`
	Detail       string `json:"detail,omitempty"`
}

func runWebCheck(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("web check", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	stackFlag := fs.String("stack", "", "the stack to install the build as (default: the bound stack, or \"web\")")
	staticOnly := fs.Bool("static-only", false, "check a build with a server entry anyway (its server half is not checked; run the conformance kit for that)")
	jsonOut := fs.Bool("json", false, "emit the report as JSON")
	strict := fs.Bool("strict", false, "fail on warnings too")
	keep := fs.Bool("keep", false, "leave the scratch chassis running and print its URLs")
	timeout := fs.Duration("timeout", 90*time.Second, "give up after this long")
	verbose := fs.Bool("verbose", false, "show the scratch chassis's log")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco web check [flags] <abi-dir | stack>

Install a Web ABI build on a throwaway chassis and probe it the way a browser
and a careless client would: a public file (ETag, body), a "_" file, an
immutable file's cache header, an unknown page, an unknown asset, a POST, HEAD
requests and a conditional GET. It catches a build whose ops leave a request
unanswered (no terminal op), "_" files that won't be served, and a bad
manifest — before anything is deployed.

<abi-dir> is a build directory (it holds txco-web.json); a <stack> name uses
the build txco.yaml binds to it. The scratch chassis runs on free ports under a
temporary directory and never touches a chassis you run yourself.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	abiDir, stack, err := resolveWebCheckTarget(fs.Arg(0), *stackFlag)
	if err != nil {
		fmt.Fprintf(stderr, "web check: %v\n", err)
		return 2
	}
	rep := &webCheckReport{ABIDir: abiDir, Stack: stack}
	finish := func() int {
		rep.Summary = map[string]int{"pass": 0, "warn": 0, "fail": 0, "skip": 0}
		for _, p := range rep.Probes {
			rep.Summary[p.Result]++
		}
		rep.OK = len(rep.Problems) == 0 && rep.Summary["fail"] == 0 && (!*strict || (rep.Summary["warn"] == 0 && len(rep.Warnings) == 0))
		if *jsonOut {
			if err := writeJSON(stdout, rep); err != nil {
				fmt.Fprintf(stderr, "web check: encode json: %v\n", err)
				return 1
			}
		} else {
			printWebCheck(stdout, rep)
		}
		if rep.OK {
			return 0
		}
		return 1
	}

	// Offline: the build itself.
	d, ops, problems := checkABIOffline(abiDir, stack, *staticOnly)
	rep.Problems = problems
	if d != nil {
		rep.Manifest = webCheckMan{ABI: d.Manifest.ABI, Immutable: d.Manifest.Immutable}
		if d.Manifest.Server != nil {
			rep.Manifest.ServerEntry = d.Manifest.Server.Entry
			if *staticOnly {
				rep.Warnings = append(rep.Warnings, "the server half is not checked (--static-only); run the @txco/web-abi conformance kit against server/")
			}
		}
		rep.Warnings = append(rep.Warnings, d.Warnings...)
	}
	if len(rep.Problems) > 0 {
		return finish()
	}

	// A scratch chassis: free ports, an empty workspace (so files come from
	// the tenant layer, as on a fleet), its own TXCO_HOME, open admin auth,
	// and private fields shown so an unanswered request is recognisable.
	tmp, err := os.MkdirTemp("", "txco-webcheck-")
	if err != nil {
		fmt.Fprintf(stderr, "web check: temp dir: %v\n", err)
		return 1
	}
	if !*keep {
		defer os.RemoveAll(tmp)
	}
	for k, v := range map[string]string{
		"TXCO_HOME":              filepath.Join(tmp, "home"),
		"TXCO_AUTH_MODE":         "both",
		"TXCO_ADMIN_USER":        "",
		"TXCO_ADMIN_PASS":        "",
		"TXCO_WEB_DEBUG":         "SHOW_PRIVATE_VARS",
		"TXCO_DEBUG_PRIVATE":     "true",
		"TXCO_DEBUG_BREAKPOINTS": "false",
		"TXCO_TRACE_ASYNC":       "false",
		"TXCO_LOG_LEVEL":         "warn",
	} {
		_ = os.Setenv(k, v)
	}
	adminAddr, err := freeAddr()
	if err == nil {
		var webAddr string
		webAddr, err = freeAddr()
		if err == nil {
			err = runWebCheckLive(rep, tmp, adminAddr, webAddr, d, ops, *timeout, *keep, *verbose, stdout, stderr)
		}
	}
	if err != nil {
		rep.Problems = append(rep.Problems, err.Error())
	}
	return finish()
}

func runWebCheckLive(rep *webCheckReport, tmp, adminAddr, webAddr string, d *webabi.Dir, ops []bundle.Op,
	timeout time.Duration, keep, verbose bool, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var logw io.Writer = io.Discard
	if verbose {
		logw = stderr
	} else if f, err := os.Create(filepath.Join(tmp, "chassis.log")); err == nil {
		defer f.Close()
		logw = f
	}
	var started []*devpkg.Process
	var proc *devpkg.Process
	exe := os.Getenv("TXCO_WEB_CHECK_BIN") // tests: a built txco
	adminURL, webURL, err := startChassisFn(ctx, chassisOpts{
		Workspace: tmp, AdminAddr: adminAddr, WebAddr: webAddr,
		Stdout: logw, Stderr: logw, Started: &started, Out: &proc, Executable: exe,
	})
	defer func() {
		if keep {
			return
		}
		// Only what this command started: never anything by port.
		for i := len(started) - 1; i >= 0; i-- {
			started[i].Stop(5 * time.Second)
		}
	}()
	if err != nil {
		return fmt.Errorf("start a scratch chassis: %w", err)
	}
	if err := devpkg.WaitHealthy(ctx, webURL+"/healthz", 20*time.Second, 200*time.Millisecond); err != nil {
		return fmt.Errorf("scratch chassis web head: %w", err)
	}

	c := client.New(client.Target{Addr: adminURL, Tenant: "default"})
	inst, err := installForCheck(ctx, c, tmp, rep.Stack, d, ops)
	if err != nil {
		return fmt.Errorf("install: %w", err)
	}
	rep.Install = inst
	rep.Probes = runWebProbes(ctx, webURL, d)
	if keep {
		fmt.Fprintf(stdout, "scratch chassis kept: admin %s, web %s (stack %s on localhost); its directory is %s — stop it yourself\n",
			adminURL, webURL, rep.Stack, tmp)
	}
	return nil
}

// resolveWebCheckTarget reads the command's argument: a build directory, or
// a stack whose binding in txco.yaml names one.
func resolveWebCheckTarget(arg, stackFlag string) (abiDir, stack string, err error) {
	if fi, serr := os.Stat(filepath.Join(arg, webabi.ManifestName)); serr == nil && !fi.IsDir() {
		abs, aerr := filepath.Abs(arg)
		if aerr != nil {
			return "", "", aerr
		}
		stack = stackFlag
		if stack == "" {
			stack = "web"
		}
		return abs, stack, nil
	}
	dir, err := workspaceDir("")
	if err != nil {
		return "", "", fmt.Errorf("%s isn't a Web ABI directory (no %s), and no workspace to look a stack up in: %v", arg, webabi.ManifestName, err)
	}
	bindings, err := loadStackBindings(dir)
	if err != nil {
		return "", "", err
	}
	b, ok := bindings[arg]
	if !ok {
		return "", "", fmt.Errorf("%s isn't a Web ABI directory (no %s) or a stack bound in txco.yaml stacks:", arg, webabi.ManifestName)
	}
	stack = arg
	if stackFlag != "" {
		stack = stackFlag
	}
	return b.Abs, stack, nil
}

// checkABIOffline loads the build and checks everything knowable without a
// chassis: the manifest, the tree, the server-entry gate, and every op's txcl.
func checkABIOffline(abiDir, stack string, staticOnly bool) (*webabi.Dir, []bundle.Op, []string) {
	d, err := webabi.Load(abiDir)
	if err != nil {
		return nil, nil, []string{err.Error()}
	}
	ops, err := bundle.WalkABI(os.DirFS(d.Path), stack, d.Path)
	if err != nil {
		return d, nil, []string{err.Error()}
	}
	var problems []string
	if d.HasServer && !staticOnly {
		problems = append(problems, fmt.Sprintf("the build has a server entry (%s), and no chassis runs server/ yet — pass --static-only to check its static half", d.Manifest.Server.Entry))
	}
	for _, op := range ops {
		where := fmt.Sprintf("%s (%d/%s)", op.SourcePath, op.Scope, op.Name)
		if _, err := txcl.Resonator(op.Txcl); err != nil {
			problems = append(problems, where+": "+err.Error())
			continue
		}
		for _, m := range txcl.Validate(op.Txcl) {
			problems = append(problems, where+": "+m)
		}
		if bad := oprefs.MalformedExecRefs(op.Txcl); len(bad) > 0 {
			problems = append(problems, where+": "+strings.Join(bad, ", ")+": "+oprefs.MalformedHint)
		}
	}
	return d, ops, problems
}

// installForCheck deploys the build, alone, as stack on the scratch chassis
// and routes localhost to it.
func installForCheck(ctx context.Context, c *client.Client, tmp, stack string, d *webabi.Dir, ops []bundle.Op) (*webCheckInst, error) {
	resolved, built, err := resolveOpRefsColocated(ops, map[string]oprefs.Operation{}, tmp, io.Discard)
	if err != nil {
		return nil, err
	}
	if err := uploadComputes(ctx, c, built, io.Discard, io.Discard); err != nil {
		return nil, err
	}
	ws := &localWorkspace{Dir: tmp, Ops: resolved,
		Bindings: map[string]stackBinding{stack: {Stack: stack, ABI: d.Path, Abs: d.Path}},
		ABI:      map[string]*webabi.Dir{stack: d}, Broken: map[string]error{}}
	b, err := buildStackFiles(tmp, stack, resolved, ws.withABI(stack, collectOpts{}))
	if err != nil {
		return nil, err
	}
	if err := b.ensureResident(ctx, c, io.Discard, io.Discard); err != nil {
		return nil, err
	}
	n, err := c.CreateDraft(ctx, stack, "active")
	if err != nil {
		return nil, fmt.Errorf("create draft: %w", err)
	}
	if _, err := c.PutDraftFiles(ctx, stack, n, b.Files); err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	vr, err := c.ValidateVersion(ctx, stack, n)
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	if !vr.OK {
		msgs := make([]string, 0, len(vr.Errors))
		for _, e := range vr.Errors {
			msgs = append(msgs, e.Path+": "+e.Err)
		}
		return nil, fmt.Errorf("the chassis refused the build:\n  %s", strings.Join(msgs, "\n  "))
	}
	if _, err := c.Activate(ctx, stack, n); err != nil {
		return nil, fmt.Errorf("activate: %w", err)
	}
	if _, err := c.AddHostname(ctx, client.AddHostnameRequest{Hostname: "localhost", Stack: stack}); err != nil {
		return nil, fmt.Errorf("route localhost: %w", err)
	}
	return &webCheckInst{Version: n, Files: len(b.Files), Ops: len(ops), Markers: len(d.Markers())}, nil
}

// probeFacts is what a probe saw.
type probeFacts struct {
	status       int
	ctype        string
	cacheControl string
	etag         string
	body         []byte
}

// unanswered reports a response no op wrote: the web head's default, a 200
// with the envelope itself as JSON (private fields shown on the scratch
// chassis, so its request is visible). A HEAD carries no body to look into.
func (f probeFacts) unanswered(head bool) bool {
	if f.status != http.StatusOK || mediaType(f.ctype) != "application/json" {
		return false
	}
	return head || gjson.GetBytes(f.body, "_txc.web.req").Exists()
}

func mediaType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(ct))
	}
	return mt
}

// webProbe is one request and how to judge its answer.
type webProbe struct {
	name, method, path string
	header             http.Header
	skip               string // non-empty: not applicable, and why
	judge              func(f probeFacts) (result, detail string)
}

// planWebProbes builds the probe list for a build. Pure: the HTTP is
// runWebProbes'.
func planWebProbes(d *webabi.Dir, tok string, readPublic func(rel string) ([]byte, error)) []webProbe {
	isHTML := func(rel string) bool { return strings.HasSuffix(rel, ".html") || strings.HasSuffix(rel, ".htm") }
	immutable := d.Manifest.ImmutablePrefixes()
	underImmutable := func(rel string) bool {
		for _, p := range immutable {
			if strings.HasPrefix(rel, p) {
				return true
			}
		}
		return false
	}
	pick := func(ok func(rel string) bool) string {
		for _, rel := range d.Public {
			if ok(rel) {
				return rel
			}
		}
		return ""
	}
	known := pick(func(r string) bool { return static.PrivateRoot(r) == "" && !underImmutable(r) && !isHTML(r) })
	if known == "" {
		known = pick(func(r string) bool { return static.PrivateRoot(r) == "" && !underImmutable(r) })
	}
	underscore := pick(func(r string) bool { return static.PrivateRoot(r) != "" })
	imm := pick(func(r string) bool { return underImmutable(r) && !isHTML(r) })
	hasIndex := pick(func(r string) bool { return r == "index.html" }) != ""

	sameFile := func(rel string) func(f probeFacts) (string, string) {
		return func(f probeFacts) (string, string) {
			want, err := readPublic(rel)
			if err != nil {
				return "fail", "read the local file: " + err.Error()
			}
			sum := sha256.Sum256(want)
			etag := `"` + hex.EncodeToString(sum[:]) + `"`
			switch {
			case f.status != http.StatusOK:
				return "fail", fmt.Sprintf("status %d, want 200", f.status)
			case !bytes.Equal(f.body, want):
				return "fail", "the body isn't the file in public/"
			case f.etag != etag:
				return "fail", "ETag " + f.etag + ", want the content hash " + etag
			}
			return "pass", ""
		}
	}
	nav := "/txco-web-check-" + tok
	var probes []webProbe

	probes = append(probes, webProbe{name: "root", method: http.MethodGet, path: "/", judge: func(f probeFacts) (string, string) {
		if f.unanswered(false) {
			return "fail", "no op answered / (and public/ has no index.html)"
		}
		if hasIndex {
			want, _ := readPublic("index.html")
			if f.status != http.StatusOK || !bytes.Equal(f.body, want) {
				return "fail", "/ isn't public/index.html"
			}
		}
		return "pass", ""
	}})

	p := webProbe{name: "known-file", method: http.MethodGet}
	if known == "" {
		p.skip = "public/ has no plain file"
	} else {
		p.path = "/" + known
		inner := sameFile(known)
		p.judge = func(f probeFacts) (string, string) {
			r, detail := inner(f)
			if r == "pass" && strings.Contains(f.cacheControl, "immutable") {
				return "warn", "served as immutable, but it isn't under an immutable prefix"
			}
			return r, detail
		}
	}
	probes = append(probes, p)

	p = webProbe{name: "underscore", method: http.MethodGet}
	if underscore == "" {
		p.skip = `public/ has no "_" path`
	} else {
		p.path = "/" + underscore
		inner := sameFile(underscore)
		p.judge = func(f probeFacts) (string, string) {
			if r, detail := inner(f); r != "pass" {
				return "fail", detail + ` — a "_" file of the build isn't served (is the chassis older than Web ABI markers?)`
			}
			return "pass", ""
		}
	}
	probes = append(probes, p)

	p = webProbe{name: "immutable", method: http.MethodGet}
	if imm == "" {
		p.skip = "no immutable prefix with a file in it"
	} else {
		p.path = "/" + imm
		p.judge = func(f probeFacts) (string, string) {
			if f.status != http.StatusOK || f.cacheControl != "public, max-age=31536000, immutable" {
				return "fail", fmt.Sprintf("status %d, Cache-Control %q; want 200 and public, max-age=31536000, immutable", f.status, f.cacheControl)
			}
			return "pass", ""
		}
	}
	probes = append(probes, p)

	probes = append(probes, webProbe{name: "navigation", method: http.MethodGet, path: nav,
		header: http.Header{"Accept": {"text/html"}}, judge: func(f probeFacts) (string, string) {
			switch {
			case f.unanswered(false):
				return "fail", "no op answered an unknown page — the build needs a terminal op (an SPA fallback, a 404 page, or a catch-all)"
			case (f.status == 200 || f.status == 404) && mediaType(f.ctype) == "text/html":
				return "pass", ""
			}
			return "warn", fmt.Sprintf("answered %d %s; a browser navigation usually wants HTML", f.status, f.ctype)
		}})
	probes = append(probes, webProbe{name: "dotted", method: http.MethodGet, path: nav + ".js", judge: func(f probeFacts) (string, string) {
		switch {
		case f.unanswered(false):
			return "fail", "no op answered an unknown asset — the build needs a catch-all op"
		case f.status == 404 || f.status == 410:
			return "pass", ""
		case f.status == 200 && mediaType(f.ctype) == "text/html":
			return "warn", "a missing asset gets HTML with 200 (the shell?)"
		}
		return "warn", fmt.Sprintf("answered %d for a missing asset", f.status)
	}})
	probes = append(probes, webProbe{name: "post", method: http.MethodPost, path: nav, judge: func(f probeFacts) (string, string) {
		switch {
		case f.unanswered(false):
			return "fail", "no op answered a POST to an unknown path — the build needs a catch-all op"
		case f.status >= 400 && f.status < 500:
			return "pass", ""
		}
		return "warn", fmt.Sprintf("answered %d to a POST nothing handles", f.status)
	}})
	probes = append(probes, webProbe{name: "head-nav", method: http.MethodHead, path: nav})
	p = webProbe{name: "head-file", method: http.MethodHead}
	if known == "" {
		p.skip = "public/ has no plain file"
	} else {
		p.path = "/" + known
	}
	probes = append(probes, p)
	p = webProbe{name: "conditional", method: http.MethodGet}
	if known == "" {
		p.skip = "public/ has no plain file"
	} else {
		want, _ := readPublic(known)
		sum := sha256.Sum256(want)
		p.path = "/" + known
		p.header = http.Header{"If-None-Match": {`W/"` + hex.EncodeToString(sum[:]) + `"`}}
		p.judge = func(f probeFacts) (string, string) {
			if f.status != http.StatusNotModified {
				return "fail", fmt.Sprintf("a weak If-None-Match got %d, want 304", f.status)
			}
			return "pass", ""
		}
	}
	probes = append(probes, p)
	return probes
}

// runWebProbes sends every probe to the scratch chassis's web head.
func runWebProbes(ctx context.Context, webURL string, d *webabi.Dir) []probeResult {
	tok := make([]byte, 4)
	_, _ = rand.Read(tok)
	readPublic := func(rel string) ([]byte, error) {
		return os.ReadFile(filepath.Join(d.Path, "public", filepath.FromSlash(rel)))
	}
	probes := planWebProbes(d, hex.EncodeToString(tok), readPublic)
	hc := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	byName := map[string]probeResult{}
	var out []probeResult
	for _, p := range probes {
		r := probeResult{Name: p.name, Method: p.method, Path: p.path}
		if p.skip != "" {
			r.Result, r.Detail = "skip", p.skip
			out = append(out, r)
			continue
		}
		f, err := fetchProbe(ctx, hc, webURL, p)
		if err != nil {
			r.Result, r.Detail = "fail", err.Error()
			out = append(out, r)
			byName[p.name] = r
			continue
		}
		r.Status, r.ContentType, r.CacheControl, r.ETag = f.status, f.ctype, f.cacheControl, f.etag
		switch {
		case p.judge != nil:
			r.Result, r.Detail = p.judge(f)
		case p.name == "head-nav":
			r.Result, r.Detail = judgeHeadTwin(f, byName["navigation"])
		case p.name == "head-file":
			r.Result, r.Detail = judgeHeadTwin(f, byName["known-file"])
		}
		byName[p.name] = r
		out = append(out, r)
	}
	return out
}

// judgeHeadTwin: a HEAD answers like its GET, with no body.
func judgeHeadTwin(f probeFacts, get probeResult) (string, string) {
	switch {
	case f.unanswered(true) && get.Status != http.StatusOK:
		return "fail", "no op answered the HEAD"
	case len(f.body) > 0:
		return "fail", "a HEAD answer carries a body"
	case get.Status != 0 && f.status != get.Status:
		return "fail", fmt.Sprintf("HEAD got %d, GET got %d", f.status, get.Status)
	case get.ETag != "" && f.etag != get.ETag:
		return "fail", "HEAD and GET disagree on the ETag"
	}
	return "pass", ""
}

func fetchProbe(ctx context.Context, hc *http.Client, base string, p webProbe) (probeFacts, error) {
	u := base + (&url.URL{Path: p.path}).EscapedPath()
	var body io.Reader
	if p.method == http.MethodPost {
		body = strings.NewReader("{}")
	}
	req, err := http.NewRequestWithContext(ctx, p.method, u, body)
	if err != nil {
		return probeFacts{}, err
	}
	for k, vs := range p.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if p.method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return probeFacts{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return probeFacts{status: resp.StatusCode, ctype: resp.Header.Get("Content-Type"),
		cacheControl: resp.Header.Get("Cache-Control"), etag: resp.Header.Get("ETag"), body: b}, nil
}

func printWebCheck(w io.Writer, rep *webCheckReport) {
	fmt.Fprintf(w, "web check: %s as stack %s\n", rep.ABIDir, rep.Stack)
	for _, p := range rep.Problems {
		fmt.Fprintf(w, "  ✗ %s\n", p)
	}
	for _, wn := range rep.Warnings {
		fmt.Fprintf(w, "  ⚠ %s\n", wn)
	}
	if rep.Install != nil {
		fmt.Fprintf(w, "  installed v%d: %d files, %d ops, %d markers\n", rep.Install.Version, rep.Install.Files, rep.Install.Ops, rep.Install.Markers)
	}
	mark := map[string]string{"pass": "✓", "warn": "⚠", "fail": "✗", "skip": "–"}
	for _, p := range rep.Probes {
		line := fmt.Sprintf("  %s %-11s %-4s %s", mark[p.Result], p.Name, p.Method, p.Path)
		if p.Status != 0 {
			line += fmt.Sprintf("  → %d %s", p.Status, mediaType(p.ContentType))
		}
		if p.Detail != "" {
			line += "  (" + p.Detail + ")"
		}
		fmt.Fprintln(w, line)
	}
	keys := []string{"pass", "warn", "fail", "skip"}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", rep.Summary[k], k))
	}
	verdict := "ok"
	if !rep.OK {
		verdict = "FAILED"
	}
	fmt.Fprintf(w, "web check: %s (%s)\n", verdict, strings.Join(parts, ", "))
}
