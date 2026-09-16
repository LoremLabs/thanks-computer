package webdav

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
)

// LOCK / UNLOCK are a stateless shim, not a lock manager.
//
// macOS Finder mounts a server read-only unless OPTIONS advertises DAV
// class 2, and then brackets every write in LOCK … UNLOCK. The head
// answers LOCK with a fresh opaquelocktoken and the lockdiscovery body a
// client expects, and UNLOCK with 204 — and verifies nothing: no token is
// stored, no node consulted, no writer excluded. Two clients editing one
// file both "hold" a lock; the real protection is the etag on
// If-Match / If-None-Match, which the store enforces on every write.
//
// This is honest about what a lock can be on a fleet with no shared lock
// state and makes the head stateless across nodes by construction. A
// LOCK on an unmapped URL creates an empty file first (RFC 4918 §9.10.4
// requires it; Finder LOCKs a new file before its first PUT) and answers
// 201.

// lockInfo is the LOCK request body (RFC 4918 §14.11); only owner is
// echoed back.
type lockInfo struct {
	XMLName xml.Name `xml:"DAV: lockinfo"`
	Owner   *struct {
		Inner string `xml:",innerxml"`
	} `xml:"DAV: owner"`
}

// maxLockBody bounds the LOCK request body read.
const maxLockBody = 64 << 10

func (c *Controller) serveLock(w http.ResponseWriter, r *http.Request, pr principal) {
	rel := c.rel(r.URL.Path)
	// A refresh (If: (<token>), no body) keeps the client's token; a new
	// lock mints one.
	token := ""
	if ifh := r.Header.Get("If"); ifh != "" {
		if i := strings.Index(ifh, "opaquelocktoken:"); i >= 0 {
			end := strings.IndexAny(ifh[i:], ">) ")
			if end < 0 {
				end = len(ifh) - i
			}
			token = ifh[i : i+end]
		}
	}
	var owner string
	if r.ContentLength != 0 {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxLockBody))
		if err != nil {
			http.Error(w, "bad lock body", http.StatusBadRequest)
			return
		}
		if len(strings.TrimSpace(string(body))) > 0 {
			var li lockInfo
			if err := xml.Unmarshal(body, &li); err != nil {
				http.Error(w, "bad lock body", http.StatusBadRequest)
				return
			}
			if li.Owner != nil {
				owner = li.Owner.Inner
			}
		}
	}
	created := false
	res, found, err := c.store.Stat(r.Context(), pr.coll.ID, rel)
	if err != nil {
		serveStoreError(w, err)
		return
	}
	if !found {
		if token != "" {
			http.Error(w, "no such resource", http.StatusNotFound)
			return
		}
		if _, err := c.store.Put(r.Context(), pr.coll.ID, rel, strings.NewReader(""), 0, chdrive.PutOpts{}); err != nil {
			serveStoreError(w, err)
			return
		}
		res, _, _ = c.store.Stat(r.Context(), pr.coll.ID, rel)
		created = true
	}
	if token == "" {
		token = "opaquelocktoken:" + uuid.NewString()
	}
	depth := "0"
	if d := strings.TrimSpace(r.Header.Get("Depth")); d != "" && res.IsDir() {
		depth = d
	}
	timeout := "Second-" + strconv.Itoa(int(lockTimeout.Seconds()))
	if t := strings.TrimSpace(r.Header.Get("Timeout")); strings.HasPrefix(t, "Second-") {
		if n, err := strconv.Atoi(strings.TrimPrefix(strings.SplitN(t, ",", 2)[0], "Second-")); err == nil && n > 0 && n <= int(lockTimeout.Seconds()) {
			timeout = "Second-" + strconv.Itoa(n)
		}
	}
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<D:prop xmlns:D="DAV:"><D:lockdiscovery><D:activelock>`)
	b.WriteString(`<D:locktype><D:write/></D:locktype><D:lockscope><D:exclusive/></D:lockscope>`)
	fmt.Fprintf(&b, `<D:depth>%s</D:depth>`, xmlEscape(depth))
	if owner != "" {
		fmt.Fprintf(&b, `<D:owner>%s</D:owner>`, owner)
	}
	fmt.Fprintf(&b, `<D:timeout>%s</D:timeout>`, timeout)
	fmt.Fprintf(&b, `<D:locktoken><D:href>%s</D:href></D:locktoken>`, xmlEscape(token))
	fmt.Fprintf(&b, `<D:lockroot><D:href>%s</D:href></D:lockroot>`, xmlEscape(c.href(res)))
	b.WriteString(`</D:activelock></D:lockdiscovery></D:prop>`)

	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.Header().Set("Lock-Token", "<"+token+">")
	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = io.WriteString(w, b.String())
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
