package contacts

import (
	"encoding/xml"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
)

// objects.go — PUT, GET/HEAD and addressbook-multiget, answered by the
// head from stored bytes. go-webdav's carddav server decodes a PUT with
// go-vcard and re-encodes every `address-data` with go-vcard's encoder,
// which reorders parameters, never escapes `;`, and turns an escaped `\;`
// into `\\;` on the way back — so what Apple Contacts wrote would not be
// what it read. Here a client's bytes (CRLF-normalized, validated by the
// chassis's own tokenizer) ARE the object; the etag is their sha256; the
// no-op rule ignores REV/PRODID; and the same bytes come back on GET and
// in a multiget. The library still serves listing, OPTIONS, MKCOL, DELETE
// routing and addressbook-query over backend.go.

const contentTypeVCard = "text/vcard; charset=utf-8"

// davError writes a WebDAV error body: an optional CardDAV precondition
// element and the message as DAV:responsedescription (what Apple shows).
func davError(w http.ResponseWriter, status int, precondition, msg string) {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<D:error xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">`)
	if precondition != "" {
		b.WriteString(`<C:` + precondition + `/>`)
	}
	b.WriteString(`<D:responsedescription>` + xmlEscape(msg) + `</D:responsedescription></D:error>`)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(b.String()))
}

// condHeader reads an If-Match / If-None-Match value: "" (absent), "*",
// or the bare etag (quotes and a weak prefix dropped).
func condHeader(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "*" {
		return v
	}
	v = strings.TrimPrefix(v, "W/")
	return strings.Trim(v, `"`)
}

func (c *Controller) bookAt(w http.ResponseWriter, r *http.Request, pr principal, name string) (chcon.Addressbook, bool) {
	ab, found, err := c.store.GetAddressbook(r.Context(), pr.tenant, pr.username, name)
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return chcon.Addressbook{}, false
	}
	if !found {
		http.NotFound(w, r)
		return chcon.Addressbook{}, false
	}
	return ab, true
}

// servePut is PUT on an address object.
func (c *Controller) servePut(w http.ResponseWriter, r *http.Request, pr principal, parts []string) {
	ab, ok := c.bookAt(w, r, pr, parts[2])
	if !ok {
		return
	}
	resource := parts[3]
	if !chcon.ValidObjectName(resource) {
		http.Error(w, "resource name is not a URL segment", http.StatusForbidden)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		if t, _, err := mime.ParseMediaType(ct); err != nil || (t != "text/vcard" && t != "text/x-vcard" && t != "text/directory") {
			davError(w, http.StatusUnsupportedMediaType, "supported-address-data", "send text/vcard")
			return
		}
	}
	if r.ContentLength > c.maxBytes {
		davError(w, http.StatusRequestEntityTooLarge, "max-resource-size", fmt.Sprintf("object over %d bytes", c.maxBytes))
		return
	}
	body, err := readAllCapped(w, r, c.maxBytes)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			davError(w, http.StatusRequestEntityTooLarge, "max-resource-size", fmt.Sprintf("object over %d bytes", c.maxBytes))
			return
		}
		http.Error(w, "unreadable request body", http.StatusBadRequest)
		return
	}
	bytes := chcon.Normalize(body)
	facts, err := chcon.Parse(bytes)
	if err != nil {
		if errors.Is(err, chcon.ErrUnsupportedVersion) {
			davError(w, http.StatusUnsupportedMediaType, "supported-address-data", err.Error())
			return
		}
		davError(w, http.StatusBadRequest, "valid-address-data", err.Error())
		return
	}
	existing, exists, err := c.store.GetObject(r.Context(), ab.ID, resource)
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	m := mutation{tenant: pr.tenant, account: pr.username, op: opPut, addressbook: refOf(ab),
		object: &objRef{Name: resource, UID: facts.UID, Size: int64(len(bytes)), Exists: exists},
		vcard:  bytes, card: &facts, clientIP: pr.clientIP}
	if exists {
		m.object.PriorETag = existing.ETag
		if pc, err := chcon.Parse(existing.VCard); err == nil {
			m.prior = &pc
		}
	}
	if status, msg := c.gate(&ab, &pr.acct, chcon.VerbPut, &m); status != 0 {
		davError(w, status, "", msg)
		return
	}
	if m.rewrite != nil {
		// The stack's version replaces the client's bytes; the UID stays
		// the client's so its resource keeps its identity.
		switch {
		case m.rewrite.card != nil:
			card := *m.rewrite.card
			card.UID = facts.UID
			if bytes, err = chcon.Render(card, c.now()); err != nil {
				http.Error(w, "stack rewrite did not render: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		case len(m.rewrite.vcard) > 0:
			bytes = chcon.Normalize(m.rewrite.vcard)
		}
		if facts, err = chcon.Parse(bytes); err != nil {
			http.Error(w, "stack rewrite did not parse: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if facts.UID != m.object.UID {
			http.Error(w, "stack rewrite changed the UID", http.StatusServiceUnavailable)
			return
		}
	}
	res, err := c.store.PutObject(r.Context(), ab.ID, chcon.Object{
		Name: resource, UID: facts.UID, VCard: bytes, Size: int64(len(bytes)),
		Version: facts.Version, FN: facts.FN, Kind: facts.Kind, Addresses: facts.Addresses,
	}, chcon.PutOpts{IfMatch: condHeader(r.Header.Get("If-Match")), IfNoneMatch: condHeader(r.Header.Get("If-None-Match"))})
	switch {
	case errors.Is(err, chcon.ErrPrecondition):
		http.Error(w, "precondition failed", http.StatusPreconditionFailed)
		return
	case errors.Is(err, chcon.ErrUIDConflict):
		davError(w, http.StatusConflict, "no-uid-conflict", "the UID names another resource")
		return
	case errors.Is(err, chcon.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	m.object.ETag = res.ETag
	m.object.Size = int64(len(bytes))
	m.vcard = bytes
	m.card = &facts
	if !res.Noop {
		c.after(&ab, &pr.acct, chcon.VerbPut, m)
	}
	w.Header().Set("ETag", `"`+res.ETag+`"`)
	if res.Created {
		w.WriteHeader(http.StatusCreated)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func readAllCapped(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	rd := http.MaxBytesReader(w, r.Body, max)
	defer rd.Close()
	var buf []byte
	tmp := make([]byte, 32<<10)
	for {
		n, err := rd.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return nil, err
		}
	}
}

// serveGet is GET/HEAD on an address object: the stored bytes, verbatim.
func (c *Controller) serveGet(w http.ResponseWriter, r *http.Request, pr principal, parts []string) {
	ab, ok := c.bookAt(w, r, pr, parts[2])
	if !ok {
		return
	}
	o, found, err := c.store.GetObject(r.Context(), ab.ID, parts[3])
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	etag := `"` + o.ETag + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", contentTypeVCard)
	w.Header().Set("Last-Modified", o.UpdatedAt.UTC().Format(http.TimeFormat))
	if inm := r.Header.Get("If-None-Match"); inm != "" && (inm == "*" || strings.Contains(inm, etag)) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(o.VCard)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(o.VCard)
	}
}

// ---- addressbook-multiget -------------------------------------------------

type multigetReq struct {
	XMLName xml.Name
	AllProp *struct{} `xml:"allprop"`
	Prop    *struct {
		Any []propElem `xml:",any"`
	} `xml:"prop"`
	Hrefs []string `xml:"href"`
}

// isMultiget reports whether a REPORT body's root is addressbook-multiget.
func isMultiget(body []byte) bool {
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local == "addressbook-multiget"
		}
	}
}

// serveMultiget answers REPORT addressbook-multiget from stored bytes:
// getetag, getcontenttype and address-data for every href in the book;
// a 404 response element for one that is not there.
func (c *Controller) serveMultiget(w http.ResponseWriter, r *http.Request, pr principal, parts []string, body []byte) {
	ab, ok := c.bookAt(w, r, pr, parts[2])
	if !ok {
		return
	}
	var req multigetReq
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, "malformed report", http.StatusBadRequest)
		return
	}
	wantETag, wantCT, wantData := true, true, true
	var missing []xml.Name
	if req.Prop != nil && req.AllProp == nil {
		wantETag, wantCT, wantData = false, false, false
		for _, p := range req.Prop.Any {
			switch p.XMLName.Space + " " + p.XMLName.Local {
			case nsDAV + " getetag":
				wantETag = true
			case nsDAV + " getcontenttype":
				wantCT = true
			case nsCardDAV + " address-data":
				wantData = true
			default:
				missing = append(missing, p.XMLName)
			}
		}
	}
	// Resolve every href to a resource name in this book; fetch them in
	// one query.
	type want struct {
		href, name string
	}
	var wants []want
	var names []string
	for _, h := range req.Hrefs {
		h = strings.TrimSpace(h)
		p := h
		if u, err := url.Parse(h); err == nil {
			p = u.Path
		}
		hp := c.pathParts(p)
		name := ""
		if len(hp) == 4 && hp[0] == pr.username && hp[1] == "addressbooks" && hp[2] == ab.Name {
			name = hp[3]
			names = append(names, name)
		}
		wants = append(wants, want{href: h, name: name})
	}
	found := map[string]chcon.Object{}
	if len(names) > 0 {
		objs, err := c.store.ListObjects(r.Context(), ab.ID, chcon.ListOpts{Names: names})
		if err != nil {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		for _, o := range objs {
			found[o.Name] = o
		}
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<D:multistatus xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">`)
	for _, wt := range wants {
		o, ok := found[wt.name]
		if wt.name == "" || !ok {
			b.WriteString(`<D:response><D:href>` + hrefEsc(wt.href) + `</D:href><D:status>HTTP/1.1 404 Not Found</D:status></D:response>`)
			continue
		}
		b.WriteString(`<D:response><D:href>` + hrefEsc(wt.href) + `</D:href><D:propstat><D:prop>`)
		if wantETag {
			b.WriteString(`<D:getetag>&quot;` + xmlEscape(o.ETag) + `&quot;</D:getetag>`)
		}
		if wantCT {
			b.WriteString(`<D:getcontenttype>` + contentTypeVCard + `</D:getcontenttype>`)
		}
		if wantData {
			b.WriteString(`<C:address-data>` + xmlEscape(string(o.VCard)) + `</C:address-data>`)
		}
		b.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>`)
		if len(missing) > 0 {
			b.WriteString(`<D:propstat><D:prop>`)
			for _, n := range missing {
				b.WriteString(fmt.Sprintf(`<x:%s xmlns:x="%s"/>`, xmlEscape(n.Local), xmlEscape(n.Space)))
			}
			b.WriteString(`</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>`)
		}
		b.WriteString(`</D:response>`)
	}
	b.WriteString(`</D:multistatus>`)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("DAV", "1, 3, addressbook")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write([]byte(b.String()))
}
