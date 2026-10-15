package webdav

import (
	"errors"
	"net/http"
	"strings"

	"github.com/emersion/go-webdav"

	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
)

// The head is the only door a WebDAV CLIENT comes through, so it is where a
// collection's policy is enforced (chassis/drive/policy.go). The stack's own
// `txco://drive/*` ops talk to the store directly and are never checked —
// that asymmetry is the point: a stack can curate a subtree its clients may
// only read.
//
// A policy is not a security boundary between principals. One account is
// bound to one collection (and what it links), so the client is always the
// same person; what the policy divides is LABOUR, not privilege.

// errPolicy is what a refused verb looks like to the client: 403, with a
// sentence a person can act on rather than a code.
func errPolicy(verb, rel string) error {
	what := map[string]string{
		chdrive.VerbWrite:   "write files in",
		chdrive.VerbCreate:  "create folders in",
		chdrive.VerbDelete:  "delete from",
		chdrive.VerbMoveIn:  "move files into",
		chdrive.VerbMoveOut: "move files out of",
	}[verb]
	if what == "" {
		what = verb + " in"
	}
	where := "this folder"
	if rel != "" {
		where = "/" + rel
	}
	return webdav.NewHTTPError(http.StatusForbidden,
		errors.New("webdav: this drive does not let a client "+what+" "+where+" — that part is kept by the stack that owns it"))
}

// clientArtifact reports whether a path's last segment is one a desktop
// client writes for its own bookkeeping rather than as content: macOS
// AppleDouble sidecars (`._name`), `.DS_Store`, and any other dot-file.
// They are exempt from a `write` denial, because Finder writes them into
// every folder it merely DISPLAYS — refusing them turns browsing a
// read-only tree into a stream of error dialogs, and a consumer that reads
// the tree skips them anyway.
func clientArtifact(rel string) bool {
	return strings.HasPrefix(chdrive.BaseOf(rel), ".")
}

// allows reports whether the collection a path lands in (the principal's
// own, or one it links: links.go) lets a client apply verb at the path
// inside it, exempting client artifacts from `write`. A linked collection
// keeps its own policy: a folder it keeps for its stack is kept here too.
func (c *Controller) allows(t target, verb string) bool {
	if verb == chdrive.VerbWrite && clientArtifact(t.rel) {
		return true
	}
	return t.coll.Policy.Allows(t.rel, verb)
}

// check is the fs adapter's form: nil when allowed, a 403 otherwise.
func (f *fs) check(t target, verb string) error {
	if f.c.allows(t, verb) {
		return nil
	}
	where := t.rel
	if t.via != "" {
		where = strings.TrimSuffix(t.via+"/"+t.rel, "/")
	}
	return errPolicy(verb, where)
}
