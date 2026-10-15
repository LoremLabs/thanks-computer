package drive

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A collection's LINKS: other collections of the same tenant shown inside
// it, each at a folder path. They exist so one login can see several
// collections at once — an account is bound to ONE collection (accounts.go),
// and a link is how that collection gathers others under it — while each
// linked collection stays itself: its own resources, policy, quota and
// mutation events. A file written at a linked path is written INTO the
// linked collection, exactly as if its own account had written it.
//
// Shape: link path → collection name.
//
//	{"teams/red": "red", "teams/blue": "blue"}
//
// reads as: the folder teams/red/ of this collection is collection "red"'s
// root, and teams/blue/ is "blue"'s. A missing parent ("teams") is shown as
// a folder holding the links; a real resource at or below a link path is
// hidden by the link.
//
// LINKS BIND THE HEAD, NEVER THE STORE, as a policy does: the WebDAV head
// follows them, `txco://drive/*` addresses each collection by its own name
// and never does. Who may set them is the stack's business (the op is the
// stack's); a link is therefore a grant — whoever signs in to this
// collection reaches the linked ones — and a stack links only what that
// login may see.
type Links map[string]string

// MaxLinks bounds a collection's links (a listing synthesizes from them).
const MaxLinks = 1000

// ParseLinks reads stored links. An empty string is no links at all, what
// every collection has until a stack sets some.
func ParseLinks(raw string) (Links, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return nil, nil
	}
	var l Links
	if err := json.Unmarshal([]byte(raw), &l); err != nil {
		return nil, fmt.Errorf("drive: links: %w", err)
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return l, nil
}

// Validate refuses links the head could not follow: a path that isn't one
// (or is the root), a name that isn't a collection's, or a link inside
// another (which one would a path below both resolve to?).
func (l Links) Validate() error {
	if len(l) > MaxLinks {
		return fmt.Errorf("drive: links: more than %d", MaxLinks)
	}
	paths := make([]string, 0, len(l))
	for at, name := range l {
		p, err := NormalizePath(at)
		if err != nil {
			return fmt.Errorf("drive: link path %q: %w", at, err)
		}
		if p == "" {
			return fmt.Errorf("drive: link path %q: the root cannot be a link", at)
		}
		if p != at {
			return fmt.Errorf("drive: link path %q: write it as %q", at, p)
		}
		if !ValidCollectionName(name) {
			return fmt.Errorf("drive: link %q: %q is not a collection name", at, name)
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for i := 1; i < len(paths); i++ {
		if IsInside(paths[i], paths[i-1]) {
			return fmt.Errorf("drive: link %q is inside link %q", paths[i], paths[i-1])
		}
	}
	return nil
}

// String serializes links for storage; no links is "".
func (l Links) String() string {
	if len(l) == 0 {
		return ""
	}
	b, err := json.Marshal(l)
	if err != nil {
		return ""
	}
	return string(b)
}

// Resolve finds the link a normalized path is at or below: its link path,
// the linked collection's name, and the path inside it ("" at the link
// itself). ok is false for a path no link covers (the collection's own).
func (l Links) Resolve(p string) (at, name, inner string, ok bool) {
	if len(l) == 0 || p == "" {
		return "", "", "", false
	}
	// Links never nest (Validate), so at most one covers p; the walk is
	// over p's own ancestors, not the links.
	for q := p; q != ""; q = ParentOf(q) {
		if n, hit := l[q]; hit {
			return q, n, strings.TrimPrefix(strings.TrimPrefix(p, q), "/"), true
		}
	}
	return "", "", "", false
}

// Under reports the names directly below dir (a normalized path, "" the
// root) that are links or lead to one — what a listing of dir shows beside
// its own children — each with whether it IS a link.
func (l Links) Under(dir string) map[string]bool {
	out := map[string]bool{}
	for at := range l {
		if !IsInside(at, dir) {
			continue
		}
		rest := strings.TrimPrefix(at, dir)
		rest = strings.TrimPrefix(rest, "/")
		seg, more, _ := strings.Cut(rest, "/")
		if more == "" {
			out[seg] = true
		} else if !out[seg] {
			out[seg] = false
		}
	}
	return out
}

// Holds reports whether any link is at p or below it (p normalized): a
// folder that holds a link cannot be deleted or moved by a client, since
// that would take a whole linked collection with it.
func (l Links) Holds(p string) bool {
	for at := range l {
		if at == p || IsInside(at, p) {
			return true
		}
	}
	return false
}

// Leads reports whether p (normalized, not itself a link) is a folder on
// the way to a link: shown as a folder even where the collection has none.
func (l Links) Leads(p string) bool {
	for at := range l {
		if IsInside(at, p) {
			return true
		}
	}
	return false
}
