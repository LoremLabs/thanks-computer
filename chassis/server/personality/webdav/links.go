package webdav

import (
	"context"
	"errors"
	"net/http"
	"path"

	"github.com/emersion/go-webdav"

	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
)

// A collection's links (chassis/drive/links.go) are followed HERE: a login
// bound to a collection that links others sees each one as a folder of its
// own tree, and a request below a link is a request to the linked
// collection — its resources, its policy, its quota, its mutation events —
// exactly as if that collection's own account had made it. So a file
// dropped through a link is filed by whatever watches the linked
// collection, and a folder the linked collection keeps for its stack is
// kept here too.
//
// What a link is NOT is a place a client can rearrange: the link itself,
// and a folder that holds one, can't be deleted or moved (that would take
// a whole collection with it); a MOVE from one collection to another is
// refused as RFC 4918 §9.9.4's cross-repository case (502 — Finder copies
// instead); a COPY across is a copy into the destination collection.

// target is where a request path lands: the collection that holds it (the
// principal's own, or one it links), the path inside it, and the link it
// came through ("" for the principal's own collection).
type target struct {
	coll chdrive.Collection
	rel  string
	via  string
	// norm is the path in the principal's own tree, normalized ("" when it
	// couldn't be: the store then answers with its own error).
	norm string
}

var (
	errLinkHeld   = errors.New("webdav: that folder holds a linked drive, which can't be deleted, moved or replaced here")
	errCrossLinks = errors.New("webdav: moving between linked drives isn't possible here; copy instead")
)

// resolve maps a request path — or a COPY/MOVE destination — to its target.
// A path no link covers is the principal's own, passed to the store as it
// came (the store normalizes it, and refuses a bad one with its own error).
func (c *Controller) resolve(ctx context.Context, pr principal, urlPath string) (target, error) {
	raw := c.rel(pr, urlPath)
	norm, err := chdrive.NormalizePath(raw)
	if err != nil {
		return target{coll: pr.coll, rel: raw}, nil
	}
	at, name, inner, ok := pr.coll.Links.Resolve(norm)
	if !ok {
		return target{coll: pr.coll, rel: raw, norm: norm}, nil
	}
	coll, found, err := c.store.GetCollection(ctx, pr.tenant, name)
	if err != nil {
		return target{}, err
	}
	if !found {
		// A link to a collection that isn't there (yet): nothing at it.
		return target{}, chdrive.ErrNotFound
	}
	return target{coll: coll, rel: inner, via: at, norm: norm}, nil
}

// own reports whether the target is in the principal's own collection.
func (t target) own() bool { return t.via == "" }

// linkPoint reports whether the target is a link itself (a linked root).
func (t target) linkPoint() bool { return t.via != "" && t.rel == "" }

// held reports whether the target is a link, or a folder of the
// principal's own tree that holds one.
func (t target) held(pr principal) bool {
	return t.linkPoint() || (t.own() && t.norm != "" && pr.coll.Links.Holds(t.norm))
}

// lead reports whether the target is a folder of the principal's own tree
// on the way to a link (shown even where the collection has no such
// folder).
func (t target) lead(pr principal) bool {
	return t.own() && pr.coll.Links.Leads(t.norm)
}

// hrefVia is href for a resource reached through a link.
func (c *Controller) hrefVia(pr principal, via string, res chdrive.Resource) string {
	if via == "" {
		return c.href(pr, res)
	}
	p := via
	if res.Path != "" {
		p = path.Join(via, res.Path)
	}
	if res.IsDir() {
		return pr.prefix + "/" + p + "/"
	}
	return pr.prefix + "/" + p
}

// leadInfo is a folder the principal's tree shows on the way to a link,
// where its own collection has none.
func (c *Controller) leadInfo(pr principal, norm string) *webdav.FileInfo {
	return &webdav.FileInfo{Path: pr.prefix + "/" + norm + "/", IsDir: true, ModTime: pr.coll.UpdatedAt}
}

func errHeld() error  { return webdav.NewHTTPError(http.StatusForbidden, errLinkHeld) }
func errCross() error { return webdav.NewHTTPError(http.StatusBadGateway, errCrossLinks) }
