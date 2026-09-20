package ipp

import (
	"crypto/sha1"
	"fmt"
	"time"

	"github.com/OpenPrinting/goipp"
)

// PrinterInfo is everything that varies between two printers' answers to
// Get-Printer-Attributes. Note what is NOT here: copies, media, sides,
// quality, resolution. Those are fixed below.
type PrinterInfo struct {
	Tenant string // identity only (printer-uuid); never sent
	Name   string // the path label: /p/<Name>
	// DisplayName is the registered printer's name for people ("Paris"), ""
	// when it has none. It is what a client that adds the printer by URI
	// (Add Printer, an `ipps://` link) names the queue: without it the queue
	// is named after the HOST, which every printer on the host shares.
	DisplayName string
	URI         string // the URI the client reached us at
	// Location is where this printer "is": the ipp host it answers on. The
	// one honest answer a virtual printer has to "which room?".
	Location string
	// MoreInfo is a web page about the printer: the zone's own site.
	MoreInfo string
	Secure   bool // TLS in front (uri-security-supported)
	Formats  []string
	Queued   int       // not-yet-delivered jobs
	UpSince  time.Time // process start (printer-up-time)
	// OperationTimeout is how long a Create-Job waits for its document
	// before the job is failed (multiple-operation-time-out) — a real
	// behaviour of this printer, so it is advertised.
	OperationTimeout time.Duration
}

// SupportedOperations is the v1 operation set, in the order advertised.
var SupportedOperations = []goipp.Op{
	goipp.OpPrintJob, goipp.OpValidateJob, goipp.OpCreateJob, goipp.OpSendDocument,
	goipp.OpCancelJob, goipp.OpGetJobAttributes, goipp.OpGetJobs, goipp.OpGetPrinterAttributes,
}

// OperationSupported reports whether op is one this inlet answers.
func OperationSupported(op goipp.Op) bool {
	for _, o := range SupportedOperations {
		if o == op {
			return true
		}
	}
	return false
}

func kw(name string, vals ...string) goipp.Attribute {
	return strAttr(name, goipp.TagKeyword, vals...)
}

func strAttr(name string, tag goipp.Tag, vals ...string) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, v := range vals {
		a.Values.Add(tag, goipp.String(v))
	}
	return a
}

func intAttr(name string, tag goipp.Tag, vals ...int) goipp.Attribute {
	a := goipp.Attribute{Name: name}
	for _, v := range vals {
		a.Values.Add(tag, goipp.Integer(v))
	}
	return a
}

func boolAttr(name string, v bool) goipp.Attribute {
	return goipp.MakeAttribute(name, goipp.TagBoolean, goipp.Boolean(v))
}

// PrinterUUID is a stable identity for (tenant, printer): macOS keys its
// queue on printer-uuid, so it must not change between requests or nodes.
// A name-based (v5-shaped) UUID over a fixed namespace string.
func PrinterUUID(tenant, printer string) string {
	sum := sha1.Sum([]byte("txco:ipp:printer:" + tenant + "/" + printer))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50 // version 5
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// PrinterAttributes is the printer-description + job-template set.
//
// THE LEAST FICTIONAL PRINTER. A print client needs a printer to have
// paper, a resolution and a copy count before it will talk to it, so this
// one has them — exactly ONE of each. Nothing is offered as a choice,
// because TxCo would ignore the choice: "17 copies" and "duplex, A4" mean
// nothing to a stack receiving a document, and a capability advertised is a
// promise. If a client needs a further attribute before it accepts the
// printer, add it here as one more boring fixed value; never as a range or
// a list the chassis then disregards. (Job-template attributes a client
// sends anyway come back in the unsupported group — see Ignored.)
func PrinterAttributes(p PrinterInfo, now time.Time) goipp.Attributes {
	formats := p.Formats
	if len(formats) == 0 {
		formats = []string{FormatPDF}
	}
	security := "none"
	if p.Secure {
		security = "tls"
	}
	up := int(now.Sub(p.UpSince).Seconds())
	if up < 1 {
		up = 1
	}
	ops := make([]int, 0, len(SupportedOperations))
	for _, o := range SupportedOperations {
		ops = append(ops, int(o))
	}
	timeout := int(p.OperationTimeout.Seconds())
	if timeout < 1 {
		timeout = 60
	}
	const media = fixedMedia
	res := goipp.Resolution{Xres: 300, Yres: 300, Units: goipp.UnitsDpi}
	info := "Print to " + p.Name
	if p.DisplayName != "" {
		info = p.DisplayName
	}

	attrs := goipp.Attributes{
		// Identity + reachability: all real.
		strAttr("printer-uri-supported", goipp.TagURI, p.URI),
		kw("uri-security-supported", security),
		kw("uri-authentication-supported", "basic"),
		strAttr("printer-name", goipp.TagName, p.Name),
		strAttr("printer-info", goipp.TagText, info),
		strAttr("printer-location", goipp.TagText, p.Location),
		strAttr("printer-make-and-model", goipp.TagText, "Thanks Computer IPP"),
		strAttr("printer-device-id", goipp.TagText, "MFG:Thanks Computer;MDL:IPP;CMD:PDF;"),
		strAttr("printer-uuid", goipp.TagURI, PrinterUUID(p.Tenant, p.Name)),
		// State: a printer that exists is idle and accepting; work waiting
		// for the bus is the only queue it has.
		intAttr("printer-state", goipp.TagEnum, 3),
		kw("printer-state-reasons", "none"),
		boolAttr("printer-is-accepting-jobs", true),
		intAttr("queued-job-count", goipp.TagInteger, p.Queued),
		intAttr("printer-up-time", goipp.TagInteger, up),
		goipp.MakeAttribute("printer-current-time", goipp.TagDateTime, goipp.Time{Time: now}),
		// Protocol: all real.
		kw("ipp-versions-supported", "1.1", "2.0"),
		intAttr("operations-supported", goipp.TagEnum, ops...),
		strAttr("charset-configured", goipp.TagCharset, "utf-8"),
		strAttr("charset-supported", goipp.TagCharset, "utf-8"),
		strAttr("natural-language-configured", goipp.TagLanguage, "en"),
		strAttr("generated-natural-language-supported", goipp.TagLanguage, "en"),
		strAttr("document-format-default", goipp.TagMimeType, formats[0]),
		strAttr("document-format-supported", goipp.TagMimeType, formats...),
		kw("compression-supported", "none"),
		kw("pdl-override-supported", "attempted"),
		boolAttr("multiple-document-jobs-supported", false),
		intAttr("multiple-operation-time-out", goipp.TagInteger, timeout),
		kw("job-creation-attributes-supported", "job-name", "document-name", "document-format"),
		// Physical capabilities: ONE fixed value each. See the doc comment.
		goipp.MakeAttribute("copies-supported", goipp.TagRange, goipp.Range{Lower: 1, Upper: 1}),
		intAttr("copies-default", goipp.TagInteger, 1),
		kw("media-supported", media),
		kw("media-default", media),
		kw("media-ready", media),
		// The same one sheet, in the collection form CUPS reads first when it
		// builds a driverless queue (hundredths of a millimetre; no margins —
		// nothing is ever clipped, because nothing is ever marked).
		goipp.MakeAttrCollection("media-col-default",
			goipp.MakeAttrCollection("media-size",
				intAttr("x-dimension", goipp.TagInteger, 21590),
				intAttr("y-dimension", goipp.TagInteger, 27940)),
			intAttr("media-top-margin", goipp.TagInteger, 0),
			intAttr("media-bottom-margin", goipp.TagInteger, 0),
			intAttr("media-left-margin", goipp.TagInteger, 0),
			intAttr("media-right-margin", goipp.TagInteger, 0)),
		kw("sides-supported", "one-sided"),
		kw("sides-default", "one-sided"),
		boolAttr("color-supported", true),
		kw("print-color-mode-supported", "color"),
		kw("print-color-mode-default", "color"),
		intAttr("print-quality-supported", goipp.TagEnum, 4),
		intAttr("print-quality-default", goipp.TagEnum, 4),
		goipp.MakeAttribute("printer-resolution-supported", goipp.TagResolution, res),
		goipp.MakeAttribute("printer-resolution-default", goipp.TagResolution, res),
		intAttr("orientation-requested-supported", goipp.TagEnum, 3),
		intAttr("orientation-requested-default", goipp.TagEnum, 3),
	}
	if p.DisplayName != "" {
		attrs.Add(strAttr("printer-dns-sd-name", goipp.TagName, p.DisplayName))
	}
	if p.MoreInfo != "" {
		attrs.Add(strAttr("printer-more-info", goipp.TagURI, p.MoreInfo))
	}
	for _, f := range formats {
		if f == FormatPDF {
			attrs.Add(kw("pdf-versions-supported", "adobe-1.7", "iso-32000-1_2008"))
		}
	}
	return attrs
}

// IPP job-state enum values (RFC 8011 §5.3.7).
const (
	jobStatePending    = 3
	jobStateProcessing = 5
	jobStateCanceled   = 7
	jobStateAborted    = 8
	jobStateCompleted  = 9
)

// JobState maps a store state onto what an IPP client sees. `delivered` is
// IPP's `completed`: the printer's work is done the moment TxCo accepts
// responsibility for the document. The stack's run is not a print job.
func JobState(state string) (enum int, reason string) {
	switch state {
	case StateReceiving:
		return jobStatePending, "job-incoming"
	case StateCommitted:
		return jobStateProcessing, "job-queued"
	case StateDelivered:
		return jobStateCompleted, "job-completed-successfully"
	case StateCanceled:
		return jobStateCanceled, "job-canceled-by-user"
	default:
		return jobStateAborted, "aborted-by-system"
	}
}

// JobAttributes is the job-description set for one job. printerURI is the
// URI the client used, so job-uri stays under the name it knows us by.
func JobAttributes(j Job, printerURI string, upSince time.Time) goipp.Attributes {
	state, reason := JobState(j.State)
	upAt := func(t time.Time) int {
		if t.IsZero() {
			return 0
		}
		if s := int(t.Sub(upSince).Seconds()); s > 0 {
			return s
		}
		return 1
	}
	// job-name is REQUIRED in a job's description and a client may omit it
	// (RFC 8011 §5.3.5: the printer then generates one). Generated for the
	// protocol only — the row and the envelope keep what the client sent.
	name := j.JobName
	if name == "" {
		name = fmt.Sprintf("job-%d", j.Number)
	}
	attrs := goipp.Attributes{
		intAttr("job-id", goipp.TagInteger, int(j.Number)),
		strAttr("job-uri", goipp.TagURI, fmt.Sprintf("%s/jobs/%d", printerURI, j.Number)),
		strAttr("job-printer-uri", goipp.TagURI, printerURI),
		intAttr("job-state", goipp.TagEnum, state),
		kw("job-state-reasons", reason),
		strAttr("job-name", goipp.TagName, name),
		strAttr("job-originating-user-name", goipp.TagName, j.RequestingUser),
		intAttr("time-at-creation", goipp.TagInteger, upAt(j.CreatedAt)),
	}
	if j.StateReason != "" {
		attrs.Add(strAttr("job-state-message", goipp.TagText, j.StateReason))
	}
	if j.Size > 0 {
		attrs.Add(intAttr("job-k-octets", goipp.TagInteger, int((j.Size+1023)/1024)))
	}
	if state >= jobStateCanceled {
		done := j.DeliveredAt
		if done.IsZero() {
			done = j.CreatedAt
		}
		attrs.Add(intAttr("time-at-completed", goipp.TagInteger, upAt(done)))
	}
	return attrs
}
