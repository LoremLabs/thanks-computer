package ipp

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	chipp "github.com/loremlabs/thanks-computer/chassis/ipp"
	printerui "github.com/loremlabs/thanks-computer/chassis/server/personality/ipp/ui"
)

// ServeHTTP answers one request on an `ipp.<x>` host.
//
// Two layers of status, as in any IPP server: HTTP codes speak about the
// TRANSPORT (not found, not authenticated, wrong method, draining); once a
// request is a well-formed, authenticated IPP operation the answer is HTTP
// 200 carrying an IPP status.
func (c *Controller) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !c.Enabled() {
		http.NotFound(w, r)
		return
	}
	t, ok := ippTarget(r.Host, r.URL.Path, c.sharedZone)
	if !ok {
		http.NotFound(w, r)
		return
	}
	site, ok, err := c.site(r.Context(), t)
	if err != nil {
		// A saturated mirror or an unreadable printer row is not "no such
		// printer": say so honestly and let the client retry.
		c.pu.Logger.Warn("ipp: tenant lookup failed", zap.String("host", t.host), zap.String("err", err.Error()))
		w.Header().Set("Retry-After", "1")
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		// ONE answer for every reason a printer is not there, given before
		// the body is read: an unknown zone, a tenant without an `_ipp`
		// stack and a label that is not a registered printer are
		// indistinguishable.
		http.NotFound(w, r)
		return
	}

	// A browser at the printer's own address (the https twin of its ipps://
	// URI) gets the printer page. It sits AFTER the existence check, so it
	// answers exactly where an IPP request would get its 401 — the page
	// tells nothing a 401 does not — and shows only what the URL already
	// says.
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && t.job == 0 {
		c.servePage(w, r, site, t)
		return
	}
	if r.Method != http.MethodPost {
		if t.job == 0 {
			w.Header().Set("Allow", "GET, HEAD, POST")
		} else {
			w.Header().Set("Allow", http.MethodPost)
		}
		http.Error(w, "this is an IPP printer: POST application/ipp", http.StatusMethodNotAllowed)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != chipp.ContentType {
		http.Error(w, "Content-Type must be application/ipp", http.StatusUnsupportedMediaType)
		return
	}

	// WHO, BEFORE WHAT. The IPP operation lives in the body, and the first
	// read of the body makes net/http send `100 Continue` — after which a
	// print client starts streaming its document. A 401 sent THEN arrives
	// mid-upload: the unread document resets the connection, the client never
	// sees the challenge, and CUPS retries the whole request, credential-less,
	// forever (measured: ~50,000 Print-Jobs in two minutes from one ipptool).
	// So everything that can refuse a request is decided from the HEADERS,
	// before a body byte is read, while the client is still waiting politely
	// for its 100:
	//
	//   a credential is offered   → verify it now
	//   none, and a document is   → challenge now (nothing was uploaded)
	//   evidently on its way
	//   none, small body          → it may be the one anonymous operation;
	//                               read it (it IS the whole body) and see
	authed := false
	switch {
	case r.Header.Get("Authorization") != "":
		who, ok := c.authenticate(w, r, site)
		if !ok {
			return
		}
		site.who = who
		authed = true
	case !c.anonAttrs || !smallBody(r):
		c.demand(w, r)
		return
	}

	// Header only: the document (if any) stays unread in r.Body.
	req, err := chipp.DecodeRequest(r.Body, 0)
	if errors.Is(err, chipp.ErrVersion) {
		c.refuse(w, r, req, goipp.StatusErrorVersionNotSupported, "")
		return
	}
	if err != nil {
		http.Error(w, "malformed IPP request", http.StatusBadRequest)
		return
	}
	op := goipp.Op(req.Code)
	c.wireLog(r, t, op, req)

	if !authed {
		// A client asks what a printer can do while ADDING it, before it has
		// a password to offer. The answer holds nothing about the tenant but
		// the printer's display name, which is what names the client's queue.
		if op == goipp.OpGetPrinterAttributes && chipp.CharsetFirst(req) && uriAgrees(req, t) {
			c.getPrinterAttributes(w, r, req, site, t, false)
			return
		}
		c.discard(w, r) // small by construction; leave nothing unread
		c.demand(w, r)
		return
	}

	if !chipp.CharsetFirst(req) {
		c.refuse(w, r, req, goipp.StatusErrorBadRequest, "attributes-charset and attributes-natural-language must come first")
		return
	}
	if !chipp.OperationSupported(op) {
		c.refuse(w, r, req, goipp.StatusErrorOperationNotSupported, "")
		return
	}
	if !uriAgrees(req, t) {
		c.refuse(w, r, req, goipp.StatusErrorNotFound, "printer-uri does not name this printer")
		return
	}

	// Draining: refuse NEW documents (print clients retry); keep answering
	// the read-only operations.
	if admission.IsDraining() && (op == goipp.OpPrintJob || op == goipp.OpSendDocument || op == goipp.OpCreateJob) {
		c.discard(w, r)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "draining", http.StatusServiceUnavailable)
		return
	}

	switch op {
	case goipp.OpGetPrinterAttributes:
		c.getPrinterAttributes(w, r, req, site, t, true)
	case goipp.OpValidateJob:
		c.validateJob(w, req)
	case goipp.OpPrintJob:
		c.receive(w, r, req, site, t, nil)
	case goipp.OpCreateJob:
		c.createJob(w, r, req, site, t)
	case goipp.OpSendDocument:
		c.sendDocument(w, r, req, site, t)
	case goipp.OpGetJobAttributes:
		c.getJobAttributes(w, r, req, site, t)
	case goipp.OpGetJobs:
		c.getJobs(w, r, req, site, t)
	case goipp.OpCancelJob:
		c.cancelJob(w, r, req, site, t)
	}
}

// --- operations ---------------------------------------------------------------

func (c *Controller) getPrinterAttributes(w http.ResponseWriter, r *http.Request, req *goipp.Message, site printerSite, t target, authed bool) {
	info := chipp.PrinterInfo{
		Tenant: site.tenant, Name: t.printer, DisplayName: site.printer.DisplayName,
		URI: printerURI(r, t), Secure: secure(r),
		Location: t.host, MoreInfo: "https://" + t.x + "/",
		Formats: c.formats, UpSince: c.upSince, OperationTimeout: c.receiveTimeout,
	}
	if authed {
		// The queue depth is the tenant's business; an anonymous caller
		// sees an idle printer.
		if n, err := c.store.CountQueued(r.Context(), site.tenant, t.printer); err == nil {
			info.Queued = n
		}
	}
	resp := chipp.Response(req, goipp.StatusOk)
	resp.Printer = chipp.FilterRequested(chipp.PrinterAttributes(info, c.now()), chipp.OpStrings(req, "requested-attributes"))
	c.respond(w, resp)
}

// acceptable is the pre-read gate shared by Validate-Job, Print-Job,
// Create-Job and Send-Document.
func (c *Controller) acceptable(req *goipp.Message) (goipp.Status, string) {
	if declared, _ := chipp.OpString(req, "document-format"); !chipp.FormatAllowed(declared, c.formats) {
		return goipp.StatusErrorDocumentFormatNotSupported, "document-format " + strconv.Quote(declared) + " is not supported"
	}
	if comp, ok := chipp.OpString(req, "compression"); ok && comp != "none" {
		return goipp.StatusErrorCompressionNotSupported, "compression is not supported"
	}
	return goipp.StatusOk, ""
}

func (c *Controller) validateJob(w http.ResponseWriter, req *goipp.Message) {
	if st, msg := c.acceptable(req); st != goipp.StatusOk {
		c.fail(w, req, st, msg)
		return
	}
	c.respond(w, c.okWithIgnored(req))
}

func (c *Controller) createJob(w http.ResponseWriter, r *http.Request, req *goipp.Message, site printerSite, t target) {
	if st, msg := c.acceptable(req); st != goipp.StatusOk {
		c.fail(w, req, st, msg)
		return
	}
	if c.busy(r, site.tenant) {
		c.fail(w, req, goipp.StatusErrorBusy, "too many documents in flight")
		return
	}
	job, err := c.store.CreateJob(r.Context(), c.newJob(r, req, site, t))
	if err != nil {
		c.storeFail(w, req, "create job", err)
		return
	}
	resp := c.okWithIgnored(req)
	resp.Job = chipp.JobAttributes(job, printerURI(r, t), c.upSince)
	c.respond(w, resp)
}

func (c *Controller) sendDocument(w http.ResponseWriter, r *http.Request, req *goipp.Message, site printerSite, t target) {
	n, ok := jobNumber(req, t)
	if !ok {
		c.refuse(w, r, req, goipp.StatusErrorBadRequest, "job-id or job-uri is required")
		return
	}
	job, err := c.store.GetJob(r.Context(), site.tenant, t.printer, n)
	if errors.Is(err, chipp.ErrJobNotFound) {
		c.refuse(w, r, req, goipp.StatusErrorNotFound, "no such job")
		return
	}
	if err != nil {
		c.discard(w, r)
		c.storeFail(w, req, "get job", err)
		return
	}
	if job.PrincipalID != "" && job.PrincipalID != site.who.Principal.ID {
		// The job is somebody else's. With one principal per printer this
		// cannot happen (the login already matched the printer's); it is
		// here so that a wider grant cannot let one person finish another's
		// job. A job from before principals ("") has no one to compare.
		c.refuse(w, r, req, goipp.StatusErrorNotAuthorized, "this job belongs to another user")
		return
	}
	if job.State != chipp.StateReceiving {
		// One document per job (multiple-document-jobs-supported=false): a
		// job that already has its document takes no second one.
		c.refuse(w, r, req, goipp.StatusErrorMultipleJobsNotSupported, "this job already has its document")
		return
	}
	c.receive(w, r, req, site, t, &job)
}

func (c *Controller) getJobAttributes(w http.ResponseWriter, r *http.Request, req *goipp.Message, site printerSite, t target) {
	n, ok := jobNumber(req, t)
	if !ok {
		c.fail(w, req, goipp.StatusErrorBadRequest, "job-id or job-uri is required")
		return
	}
	job, err := c.store.GetJob(r.Context(), site.tenant, t.printer, n)
	if errors.Is(err, chipp.ErrJobNotFound) {
		c.fail(w, req, goipp.StatusErrorNotFound, "no such job")
		return
	}
	if err != nil {
		c.storeFail(w, req, "get job", err)
		return
	}
	resp := chipp.Response(req, goipp.StatusOk)
	resp.Job = chipp.FilterRequested(chipp.JobAttributes(job, printerURI(r, t), c.upSince), chipp.OpStrings(req, "requested-attributes"))
	c.respond(w, resp)
}

func (c *Controller) getJobs(w http.ResponseWriter, r *http.Request, req *goipp.Message, site printerSite, t target) {
	which, _ := chipp.OpString(req, "which-jobs")
	limit, _ := chipp.OpInt(req, "limit")
	jobs, err := c.store.ListJobs(r.Context(), site.tenant, t.printer, which == "completed", limit)
	if err != nil {
		c.storeFail(w, req, "list jobs", err)
		return
	}
	requested := chipp.OpStrings(req, "requested-attributes")
	if len(requested) == 0 {
		requested = []string{"job-id", "job-uri"} // RFC 8011 §4.2.6.1 default
	}
	resp := chipp.Response(req, goipp.StatusOk)
	// One job group PER job: the named Job field can hold only one group,
	// so the response is assembled as explicit groups.
	groups := goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: resp.Operation}}
	uri := printerURI(r, t)
	for _, j := range jobs {
		groups.Add(goipp.Group{Tag: goipp.TagJobGroup,
			Attrs: chipp.FilterRequested(chipp.JobAttributes(j, uri, c.upSince), requested)})
	}
	resp.Groups = groups
	c.respond(w, resp)
}

func (c *Controller) cancelJob(w http.ResponseWriter, r *http.Request, req *goipp.Message, site printerSite, t target) {
	n, ok := jobNumber(req, t)
	if !ok {
		c.fail(w, req, goipp.StatusErrorBadRequest, "job-id or job-uri is required")
		return
	}
	job, canceled, err := c.store.Cancel(r.Context(), site.tenant, t.printer, n)
	switch {
	case errors.Is(err, chipp.ErrJobNotFound):
		c.fail(w, req, goipp.StatusErrorNotFound, "no such job")
	case err != nil:
		c.storeFail(w, req, "cancel job", err)
	case canceled:
		c.noteJob("canceled")
		c.respond(w, chipp.Response(req, goipp.StatusOk))
	case job.State == chipp.StateCanceled:
		c.respond(w, chipp.Response(req, goipp.StatusOk)) // already canceled: fine
	default:
		// Delivered (or mid-handoff): the document is TxCo's now. There is
		// nothing left for a printer to cancel.
		c.fail(w, req, goipp.StatusErrorNotPossible, "the job has already been delivered")
	}
}

// --- helpers --------------------------------------------------------------------

func (c *Controller) busy(r *http.Request, tenant string) bool {
	if c.maxInflight <= 0 {
		return false
	}
	n, err := c.store.CountReceiving(r.Context(), tenant)
	return err == nil && n >= c.maxInflight
}

func (c *Controller) newJob(r *http.Request, req *goipp.Message, site printerSite, t target) chipp.NewJob {
	user, _ := chipp.OpString(req, "requesting-user-name")
	name, _ := chipp.OpString(req, "job-name")
	doc, _ := chipp.OpString(req, "document-name")
	format, _ := chipp.OpString(req, "document-format")
	return chipp.NewJob{
		Tenant: site.tenant, Printer: t.printer,
		RequestingUser: clip(user, 255), JobName: clip(name, 255), DocumentName: clip(doc, 255),
		DocumentFormat: format, Host: hostWithPort(r, t), URIPath: t.uriPath(), ClientIP: c.clientIP(r),
		PrincipalID: site.who.Principal.ID, CredentialID: site.who.Credential,
	}
}

// okWithIgnored is a success response that tells the client which of its
// job-template attributes meant nothing here (see chipp.Ignored).
func (c *Controller) okWithIgnored(req *goipp.Message) *goipp.Message {
	ignored := chipp.Ignored(req)
	if len(ignored) == 0 {
		return chipp.Response(req, goipp.StatusOk)
	}
	resp := chipp.Response(req, goipp.StatusOkIgnoredOrSubstituted)
	resp.Unsupported = ignored
	return resp
}

func (c *Controller) fail(w http.ResponseWriter, req *goipp.Message, status goipp.Status, msg string) {
	resp := chipp.Response(req, status)
	chipp.StatusMessage(resp, msg)
	c.respond(w, resp)
}

// smallBody reports whether the request's body is known to be no bigger than
// an IPP header — i.e. it cannot be carrying a document worth worrying about,
// and reading it to find out the operation costs nothing. A chunked body
// (unknown length: how a print client sends a document) is never small.
func smallBody(r *http.Request) bool {
	return r.ContentLength >= 0 && r.ContentLength <= chipp.MaxHeaderBytes
}

// discard reads and drops what is left of the request body, bounded by the
// job size limit. A refusal written while the client is still sending its
// document is a refusal the client never reads (the unread bytes reset the
// connection); draining first is what makes the answer arrive. Only ever
// reached by an authenticated client, or for a body known to be small.
func (c *Controller) discard(w http.ResponseWriter, r *http.Request) {
	limit := c.maxBytes
	if limit <= 0 {
		limit = 1 << 30
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyBudget(limit)))
	if n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, limit+1)); n > limit {
		w.Header().Set("Connection", "close") // more than a whole job's worth: stop listening
	}
}

// refuse is fail for a request whose document may still be in flight.
func (c *Controller) refuse(w http.ResponseWriter, r *http.Request, req *goipp.Message, status goipp.Status, msg string) {
	c.discard(w, r)
	c.fail(w, req, status, msg)
}

func (c *Controller) storeFail(w http.ResponseWriter, req *goipp.Message, what string, err error) {
	c.pu.Logger.Warn("ipp: store error", zap.String("op", what), zap.String("err", err.Error()))
	c.fail(w, req, goipp.StatusErrorTemporary, "temporary failure")
}

func (c *Controller) respond(w http.ResponseWriter, resp *goipp.Message) {
	body, err := chipp.Ordered(resp).EncodeBytes()
	if err != nil {
		c.pu.Logger.Warn("ipp: encode response failed", zap.String("err", err.Error()))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", chipp.ContentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// printerURI is the URI the client reached this printer at, echoed back so
// it stays under the name (and port) the client already knows.
func printerURI(r *http.Request, t target) string {
	scheme := "ipp"
	if secure(r) {
		scheme = "ipps"
	}
	return scheme + "://" + hostWithPort(r, t) + t.uriPath()
}

// hostWithPort is the canonical ipp host plus whatever port the client named.
func hostWithPort(r *http.Request, t target) string {
	if i := strings.LastIndexByte(r.Host, ':'); i > 0 && !strings.HasSuffix(r.Host, "]") {
		if _, err := strconv.Atoi(r.Host[i+1:]); err == nil {
			return t.host + r.Host[i:]
		}
	}
	return t.host
}

// uriAgrees checks that the request's printer-uri / job-uri name the printer
// the URL does. Only the PATH is compared: between the client and this
// handler a front proxy may have changed the scheme, host and port.
func uriAgrees(req *goipp.Message, t target) bool {
	for _, name := range []string{"printer-uri", "job-uri"} {
		raw, ok := chipp.OpString(req, name)
		if !ok {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return false
		}
		if u.Path != t.uriPath() && !strings.HasPrefix(u.Path, t.uriPath()+"/jobs/") {
			return false
		}
	}
	return true
}

// jobNumber finds which job a job operation means: the job-id attribute,
// else the trailing number of job-uri, else the URL's own /jobs/<n>.
func jobNumber(req *goipp.Message, t target) (int64, bool) {
	if n, ok := chipp.OpInt(req, "job-id"); ok && n > 0 {
		return int64(n), true
	}
	if raw, ok := chipp.OpString(req, "job-uri"); ok {
		if i := strings.LastIndexByte(raw, '/'); i >= 0 {
			if n, err := strconv.ParseInt(raw[i+1:], 10, 32); err == nil && n > 0 {
				return n, true
			}
		}
	}
	if t.job > 0 {
		return t.job, true
	}
	return 0, false
}

// wireLog records what a client sent: the operation and attribute NAMES.
// Never values — job names, document names and user names are the tenant's.
func (c *Controller) wireLog(r *http.Request, t target, op goipp.Op, req *goipp.Message) {
	if !c.wireDebug {
		return
	}
	names := func(attrs goipp.Attributes) []string {
		out := make([]string, 0, len(attrs))
		for _, a := range attrs {
			out = append(out, a.Name)
		}
		return out
	}
	c.pu.Logger.Info("ipp wire",
		zap.String("op", op.String()), zap.String("version", req.Version.String()),
		zap.String("host", t.host), zap.String("printer", t.printer), zap.Bool("shared_front_door", t.handle != ""),
		zap.Strings("operation_attrs", names(req.Operation)),
		zap.Strings("job_attrs", names(req.Job)),
		zap.Strings("requested", chipp.OpStrings(req, "requested-attributes")),
		zap.String("document_format", first(chipp.OpString(req, "document-format"))),
		zap.Bool("authorization", r.Header.Get("Authorization") != ""),
		zap.String("expect", r.Header.Get("Expect")),
		zap.String("transfer_encoding", strings.Join(r.TransferEncoding, ",")),
		zap.Int64("content_length", r.ContentLength),
		zap.String("user_agent", r.UserAgent()))
}

func first(s string, _ bool) string { return s }

// clip bounds a client-supplied string, never leaving half a rune behind
// (the envelope is JSON; a cut multi-byte character would be invalid UTF-8).
func clip(s string, max int) string {
	if len(s) <= max {
		return strings.ToValidUTF8(s, "")
	}
	return strings.ToValidUTF8(s[:max], "")
}

// servePage answers a browser at a printer's address with the printer page
// (package ui): the printer's name and the three fields macOS's Add Printer
// › IP tab asks for, plus an ipps:// link that opens Add Printer itself. The
// address always spells out its port — IPP's default is 631, which this head
// does not listen on, and behind the edge the Host carries none.
func (c *Controller) servePage(w http.ResponseWriter, r *http.Request, site printerSite, t target) {
	scheme, port := "ipp", ":80"
	if secure(r) {
		scheme, port = "ipps", ":443"
	}
	addr := hostWithPort(r, t)
	if addr == t.host {
		addr += port
	}
	page, _ := printerui.Page(printerui.Printer{
		Name:    site.printer.Name(),
		URI:     scheme + "://" + addr + t.uriPath(),
		Address: addr,
		Queue:   strings.TrimPrefix(t.uriPath(), "/"),
	})
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(page)))
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(page)
	}
}
