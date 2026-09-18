package ipp

import (
	"bufio"
	"errors"
	"net/http"
	"time"

	"github.com/OpenPrinting/goipp"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	chipp "github.com/loremlabs/thanks-computer/chassis/ipp"
)

// receive is the document path of Print-Job (job == nil: the job is created
// here) and Send-Document (job is the `receiving` row Create-Job made).
//
//	gate → job row → stream into the CAS (hash at EOF) → ownership → commit
//	→ answer → nudge the dispatcher
//
// The document is never held: r.Body is positioned just past the IPP header,
// and from there the bytes go through a sniff window straight into
// filecas.PutStream. Every refusal that can be decided without reading the
// document IS decided without reading it.
func (c *Controller) receive(w http.ResponseWriter, r *http.Request, req *goipp.Message, site printerSite, t target, job *chipp.Job) {
	started := c.now()
	// Every refusal below this line happens while the client may still be
	// sending its document, so each one drains the body first (c.refuse) —
	// otherwise the client reads a connection reset, not an answer.
	if c.fcas == nil || c.ix == nil {
		c.refuse(w, r, req, goipp.StatusErrorTemporary, "document storage is not available")
		return
	}
	if st, msg := c.acceptable(req); st != goipp.StatusOk {
		c.refuse(w, r, req, st, msg)
		return
	}
	// A Content-Length that already exceeds the cap is refused before a byte
	// moves. (It counts the IPP header too, hence the allowance; a chunked
	// body — a print client's usual Print-Job — has none and is bounded by
	// the stream limit below.)
	if c.maxBytes > 0 && r.ContentLength > c.maxBytes+chipp.MaxHeaderBytes {
		c.refuseTooLarge(w, r, req)
		return
	}
	if job == nil {
		if c.busy(r, site.tenant) {
			c.refuse(w, r, req, goipp.StatusErrorBusy, "too many documents in flight")
			return
		}
		created, err := c.store.CreateJob(r.Context(), c.newJob(r, req, site, t))
		if err != nil {
			c.discard(w, r)
			c.storeFail(w, req, "create job", err)
			return
		}
		job = &created
	}
	abort := func(outcome, reason string) {
		_ = c.store.MarkFailed(c.ctx, job.ID, reason) // c.ctx: the request's may already be dead
		c.noteJob(outcome)
		c.logJob(job, outcome, "", 0, started)
	}

	// Escape the listener's global read timeout for THIS request: a document
	// takes as long as it takes, within a size-scaled budget.
	rc := http.NewResponseController(w)
	budget := bodyBudget(max(r.ContentLength, c.maxBytes))
	_ = rc.SetReadDeadline(time.Now().Add(budget))
	_ = rc.SetWriteDeadline(time.Now().Add(budget + time.Minute))

	var body = r.Body
	if c.maxBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, c.maxBytes)
	}
	br := bufio.NewReaderSize(body, 4*chipp.SniffBytes)
	head, _ := br.Peek(chipp.SniffBytes) // short documents: whatever there is

	declared, _ := chipp.OpString(req, "document-format")
	format, ok := chipp.Resolve(declared, head, c.formats)
	if !ok {
		abort("bad_format", "document does not match its declared format")
		c.refuse(w, r, req, goipp.StatusErrorDocumentFormatError, "the document is not "+firstNonEmpty(declared, c.formats[0]))
		return
	}

	hash, size, err := filecas.PutStream(r.Context(), c.fcas, br, c.maxBytes)
	var tooLarge *http.MaxBytesError
	switch {
	case err == nil:
	case errors.Is(err, filecas.ErrTooLarge), errors.As(err, &tooLarge):
		abort("too_large", "document exceeds the size limit")
		c.refuseTooLarge(w, r, req)
		return
	case r.Context().Err() != nil:
		// The client went away mid-document. Nothing became visible in the
		// CAS; there is nobody to answer.
		abort("client_gone", "client disconnected")
		return
	default:
		c.pu.Logger.Warn("ipp: document store failed", zap.String("job_id", job.ID), zap.String("err", err.Error()))
		abort("store_error", "document storage failed")
		c.fail(w, req, goipp.StatusErrorTemporary, "temporary failure storing the document")
		return
	}
	if size == 0 {
		abort("empty", "empty document")
		c.fail(w, req, goipp.StatusErrorDocumentFormatError, "the document is empty")
		return
	}

	// CAS first, THEN ownership (the sendmail-retain order): a crash between
	// the two leaves bytes nobody owns — invisible, and healed by a re-print.
	// The reverse order would record ownership of bytes that may not exist.
	if _, err := c.ix.PutShaIfAbsent(r.Context(), site.tenant, blob.ShaRow{
		SHA256: hash, Size: size, ContentType: format, FirstSeen: c.now(),
	}); err != nil {
		c.pu.Logger.Warn("ipp: blob ownership failed", zap.String("job_id", job.ID), zap.String("err", err.Error()))
		abort("store_error", "document ownership failed")
		c.fail(w, req, goipp.StatusErrorTemporary, "temporary failure storing the document")
		return
	}

	docName, _ := chipp.OpString(req, "document-name")
	committed, err := c.store.SetCommitted(r.Context(), job.ID, hash, size, format, clip(docName, 255))
	if err != nil {
		c.storeFail(w, req, "commit job", err)
		return
	}
	if !committed {
		// Canceled while the document streamed. The bytes are in the CAS
		// (there is no GC) but no job refers to them and nothing is sent.
		c.noteJob("canceled")
		c.fail(w, req, goipp.StatusErrorJobCanceled, "the job was canceled")
		return
	}

	// Durable. From here the job is TxCo's responsibility whatever happens
	// to this connection. Wake the dispatcher; do not wait for it.
	select {
	case c.nudge <- struct{}{}:
	default:
	}
	c.noteJob("committed")
	c.logJob(job, "committed", format, size, started)

	current, err := c.store.GetJobByID(r.Context(), job.ID)
	if err != nil {
		current = *job
		current.State = chipp.StateCommitted
	}
	resp := c.okWithIgnored(req)
	resp.Job = chipp.JobAttributes(current, printerURI(r, t), c.upSince)
	c.respond(w, resp)
}

// refuseTooLarge answers a document over the limit. It drains one more
// limit's worth so an honest client that overshot a little still reads the
// answer; past that the connection is closed (discard sets the header) and
// the unread remainder is never parsed as a request.
func (c *Controller) refuseTooLarge(w http.ResponseWriter, r *http.Request, req *goipp.Message) {
	c.discard(w, r)
	c.fail(w, req, goipp.StatusErrorRequestEntity, "the document exceeds this printer's size limit")
}

// logJob is the operational record of one job: identifiers, format, size,
// duration, outcome. NEVER the job name, document name or user name — a
// print queue's titles are exactly the things people do not want in logs.
func (c *Controller) logJob(j *chipp.Job, outcome, format string, size int64, started time.Time) {
	c.pu.Logger.Info("ipp job",
		zap.String("tenant", j.Tenant), zap.String("printer", j.Printer),
		zap.String("job_id", j.ID), zap.Int64("job_number", j.Number),
		zap.String("format", format), zap.Int64("bytes", size),
		zap.Duration("duration", c.now().Sub(started)), zap.String("outcome", outcome))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
