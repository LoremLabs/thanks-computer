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
// state and makes the head stateless across nodes by construction.
//
// A LOCK ON AN UNMAPPED URL CREATES NOTHING. It answers 200 with a token,
// exactly as it would for a file that exists, and the file comes into being
// when the client writes it. RFC 4918 §9.10.4 says such a LOCK MUST create
// an empty resource, and until 2026-09-18 this head did — which is how a
// stack that moves a file left a ghost behind: Finder still had the OLD
// name cached, previewed it (macOS takes a write lock even to read), and
// that LOCK re-created the name as a zero-byte file nobody would ever clean
// up (prod: a document dropped into a pony's Input/Knowledge/ and filed
// into Knowledge/ by the stack reappeared in the drop zone, empty). A lock
// that reserves nothing should not write anything either.
//
// What clients actually do agrees. Finder's new-file dance is LOCK → a
// ZERO-BYTE PUT → the bytes → UNLOCK: the zero-byte PUT is how a client
// materializes a file on a server where a lock on a missing name is only a
// reservation (RFC 2518's lock-null resources — Apache mod_dav, which
// webdavfs grew up against), and RFC 4918 appendix D tells clients to be
// ready for either model. So the file still appears at the same moment, by
// the client's own hand; and a client that locks a name it merely BELIEVES
// exists gets a token, then a 404 on its GET, and drops the stale entry.

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
	rel := c.rel(pr, r.URL.Path)
	// A lock is a WRITE lock (`<D:locktype><D:write/>` below), and on an
	// unmapped URL it CREATES the file. Both are writes, so a subtree whose
	// policy refuses `write` refuses the lock too.
	//
	// Refusing HERE rather than at the PUT is the difference between a
	// client that opens a file read-only and one that finds out at save
	// time: macOS Finder took a lock, read the file, released it, tried to
	// write, got a 403 and then retried until the whole volume stalled
	// (prod, 2026-09-17). A client that cannot take a write lock stops
	// asking.
	if !c.allows(pr, rel, chdrive.VerbWrite) {
		http.Error(w, "forbidden by the collection's policy", http.StatusForbidden)
		return
	}
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
		// Reserve the name; create nothing (see the file comment). The
		// one thing the write WOULD have refused is still refused here, so
		// a client learns it at the lock and not at the save: the name must
		// be a legal one inside a directory that exists (RFC 4918 §9.10.4:
		// 409 when an ancestor is missing).
		norm, nerr := chdrive.NormalizePath(rel)
		if nerr != nil || norm == "" {
			serveStoreError(w, chdrive.ErrBadPath)
			return
		}
		if parent := chdrive.ParentOf(norm); parent != "" {
			dir, ok, perr := c.store.Stat(r.Context(), pr.coll.ID, parent)
			if perr != nil {
				serveStoreError(w, perr)
				return
			}
			if !ok {
				serveStoreError(w, chdrive.ErrNoParent)
				return
			}
			if !dir.IsDir() {
				serveStoreError(w, chdrive.ErrNotDirectory)
				return
			}
		}
		res = chdrive.Resource{Path: norm, Kind: chdrive.KindFile}
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
	fmt.Fprintf(&b, `<D:lockroot><D:href>%s</D:href></D:lockroot>`, xmlEscape(c.href(pr, res)))
	b.WriteString(`</D:activelock></D:lockdiscovery></D:prop>`)

	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.Header().Set("Lock-Token", "<"+token+">")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, b.String())
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
