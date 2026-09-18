package ipp

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/OpenPrinting/goipp"
)

// MaxHeaderBytes bounds the IPP attribute block of one request. A real
// client's Print-Job header is a few hundred bytes; the cap only stops a
// hostile peer from making the decoder build an unbounded message.
const MaxHeaderBytes = 64 << 10

// ContentType is the media type every IPP request and response carries.
const ContentType = "application/ipp"

// ErrVersion is returned by DecodeRequest for an IPP major version this
// inlet does not speak.
var ErrVersion = errors.New("ipp: unsupported protocol version")

// DecodeRequest reads ONE IPP message header (the attribute groups, up to
// and including the end-of-attributes tag) from r and stops there: goipp
// reads exactly the bytes it needs, so whatever follows — the document of a
// Print-Job or Send-Document — is still unread in r for the caller to
// stream. That property is what lets the inlet hash a 200 MB document into
// the CAS without ever holding it; wire_test.go pins it.
//
// The header read is bounded by maxHeader (<= 0 → MaxHeaderBytes). The
// document is NOT bounded here.
func DecodeRequest(r io.Reader, maxHeader int64) (*goipp.Message, error) {
	if maxHeader <= 0 {
		maxHeader = MaxHeaderBytes
	}
	msg := &goipp.Message{}
	if err := msg.Decode(io.LimitReader(r, maxHeader)); err != nil {
		return nil, fmt.Errorf("ipp: decode request: %w", err)
	}
	if maj := msg.Version.Major(); maj != 1 && maj != 2 {
		return msg, ErrVersion
	}
	return msg, nil
}

// Response starts a response to req: same request-id, the request's version
// (capped at 2.0, the newest this inlet claims), and the two operation
// attributes RFC 8011 requires first in every response.
func Response(req *goipp.Message, status goipp.Status) *goipp.Message {
	v := goipp.MakeVersion(2, 0)
	id := uint32(0)
	if req != nil {
		id = req.RequestID
		if req.Version.Major() == 1 || (req.Version.Major() == 2 && req.Version.Minor() == 0) {
			v = req.Version
		}
	}
	resp := goipp.NewResponse(v, status, id)
	resp.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
	resp.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	return resp
}

// Ordered fixes the group order of a response built through the named
// fields. RFC 8011 §4.1.3: operation attributes, then UNSUPPORTED attributes,
// then the object's (job / printer) attributes. goipp's own field order puts
// the job group ahead of the unsupported group, which CUPS's ipptool rejects
// ("Attribute groups out of order"). A response that already carries explicit
// Groups (Get-Jobs: one job group per job) is left alone.
func Ordered(resp *goipp.Message) *goipp.Message {
	if resp.Groups != nil {
		return resp
	}
	groups := goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: resp.Operation}}
	if len(resp.Unsupported) > 0 {
		groups.Add(goipp.Group{Tag: goipp.TagUnsupportedGroup, Attrs: resp.Unsupported})
	}
	if len(resp.Job) > 0 {
		groups.Add(goipp.Group{Tag: goipp.TagJobGroup, Attrs: resp.Job})
	}
	if len(resp.Printer) > 0 {
		groups.Add(goipp.Group{Tag: goipp.TagPrinterGroup, Attrs: resp.Printer})
	}
	resp.Groups = groups
	return resp
}

// StatusMessage attaches the human-readable status-message.
func StatusMessage(resp *goipp.Message, text string) {
	if text == "" {
		return
	}
	resp.Operation.Add(goipp.MakeAttribute("status-message", goipp.TagText, goipp.String(text)))
}

// --- request attribute access ---------------------------------------------------

func findAttr(attrs goipp.Attributes, name string) (goipp.Attribute, bool) {
	for _, a := range attrs {
		if a.Name == name {
			return a, true
		}
	}
	return goipp.Attribute{}, false
}

// OpString returns an operation attribute's first value as a string.
func OpString(msg *goipp.Message, name string) (string, bool) {
	a, ok := findAttr(msg.Operation, name)
	if !ok || len(a.Values) == 0 {
		return "", false
	}
	return a.Values[0].V.String(), true
}

// OpStrings returns every value of a multi-valued operation attribute.
func OpStrings(msg *goipp.Message, name string) []string {
	a, ok := findAttr(msg.Operation, name)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(a.Values))
	for _, v := range a.Values {
		out = append(out, v.V.String())
	}
	return out
}

// OpInt returns an integer/enum operation attribute.
func OpInt(msg *goipp.Message, name string) (int, bool) {
	a, ok := findAttr(msg.Operation, name)
	if !ok || len(a.Values) == 0 {
		return 0, false
	}
	if n, ok := a.Values[0].V.(goipp.Integer); ok {
		return int(n), true
	}
	return 0, false
}

// OpBool returns a boolean operation attribute.
func OpBool(msg *goipp.Message, name string) (val, ok bool) {
	a, found := findAttr(msg.Operation, name)
	if !found || len(a.Values) == 0 {
		return false, false
	}
	b, isBool := a.Values[0].V.(goipp.Boolean)
	return bool(b), isBool
}

// CharsetFirst reports whether the request opens with attributes-charset
// then attributes-natural-language, as RFC 8011 §4.1.4 requires.
func CharsetFirst(msg *goipp.Message) bool {
	return len(msg.Operation) >= 2 &&
		msg.Operation[0].Name == "attributes-charset" &&
		msg.Operation[1].Name == "attributes-natural-language"
}

// groupKeywords are the requested-attributes values that name a whole group
// rather than one attribute. Returning everything for them is always legal
// (a client must ignore attributes it did not ask for) and is what every
// small IPP server does.
var groupKeywords = map[string]struct{}{
	"all": {}, "printer-description": {}, "job-template": {}, "job-description": {},
	"document-description": {}, "media-col-database": {},
}

// FilterRequested narrows attrs to the client's requested-attributes.
func FilterRequested(attrs goipp.Attributes, requested []string) goipp.Attributes {
	if len(requested) == 0 {
		return attrs
	}
	want := make(map[string]struct{}, len(requested))
	for _, r := range requested {
		r = strings.TrimSpace(r)
		if _, isGroup := groupKeywords[r]; isGroup {
			return attrs
		}
		want[r] = struct{}{}
	}
	out := make(goipp.Attributes, 0, len(want))
	for _, a := range attrs {
		if _, ok := want[a.Name]; ok {
			out = append(out, a)
		}
	}
	return out
}

// fixedJobTemplate is the ONE value this printer has for each job-template
// attribute — the same values PrinterAttributes advertises as *-supported /
// *-default. Asking for exactly that is not asking for anything.
var fixedJobTemplate = map[string]string{
	"copies":                "1",
	"media":                 fixedMedia,
	"sides":                 "one-sided",
	"print-color-mode":      "color",
	"print-quality":         "4",
	"printer-resolution":    "300dpi",
	"orientation-requested": "3",
}

// fixedMedia is the single sheet this printer "has".
const fixedMedia = "na_letter_8.5x11in"

// Ignored builds the unsupported-attributes group: every job-template
// attribute a client sent that asks for something this printer does not
// have. The printer has exactly one of everything (fixedJobTemplate), so
// `copies=1` is simply honoured, while `copies=17` — or duplex, or A4, or any
// attribute it has never heard of — comes back here with the out-of-band
// `unsupported` value and the response status becomes
// successful-ok-ignored-or-substituted-attributes: IPP's own way of saying
// "accepted; that setting meant nothing". A job is never refused over one.
func Ignored(req *goipp.Message) goipp.Attributes {
	var out goipp.Attributes
	for _, a := range req.Job {
		if want, ok := fixedJobTemplate[a.Name]; ok && len(a.Values) == 1 && a.Values[0].V.String() == want {
			continue
		}
		out.Add(goipp.MakeAttribute(a.Name, goipp.TagUnsupportedValue, goipp.Void{}))
	}
	return out
}
