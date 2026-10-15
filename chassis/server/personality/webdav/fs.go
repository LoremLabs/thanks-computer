package webdav

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/emersion/go-webdav"

	chdrive "github.com/loremlabs/thanks-computer/chassis/drive"
)

// fs adapts one principal's collection to the library's FileSystem — with
// the collections it links (links.go), each at its folder. Names arrive as
// full URL paths (the library has no prefix), so every name is resolved on
// the way in (to the collection that holds it, and the path inside it) and
// every FileInfo.Path is rebuilt on the way out; a COPY/MOVE Destination
// outside the prefix is refused. Every error is a webdav.HTTPError — the
// library answers 500 for anything else.
type fs struct {
	c  *Controller
	pr principal
}

var errOutsidePrefix = errors.New("webdav: destination is outside the drive mount")

// wrap turns a store error into the library's HTTP error.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	return webdav.NewHTTPError(storeStatus(err), err)
}

// info is a resource of the target's collection, at its href in the
// principal's tree.
func (f *fs) info(t target, res chdrive.Resource) *webdav.FileInfo {
	fi := &webdav.FileInfo{
		Path:    f.c.hrefVia(f.pr, t.via, res),
		Size:    res.Size,
		ModTime: res.UpdatedAt,
		IsDir:   res.IsDir(),
	}
	if !res.IsDir() {
		fi.MIMEType = res.ContentType
		fi.ETag = res.ETag // the library quotes it
	}
	return fi
}

func (f *fs) at(ctx context.Context, name string) (target, error) {
	t, err := f.c.resolve(ctx, f.pr, name)
	if err != nil {
		return target{}, wrap(err)
	}
	return t, nil
}

// dest validates a COPY/MOVE destination: inside the mount (and then
// resolved, through a link where it falls below one).
func (f *fs) dest(ctx context.Context, p string) (target, error) {
	if p != f.pr.prefix && !strings.HasPrefix(p, f.pr.prefix+"/") {
		return target{}, webdav.NewHTTPError(http.StatusForbidden, errOutsidePrefix)
	}
	return f.at(ctx, p)
}

func (f *fs) Stat(ctx context.Context, name string) (*webdav.FileInfo, error) {
	t, err := f.at(ctx, name)
	if err != nil {
		return nil, err
	}
	res, found, err := f.c.store.Stat(ctx, t.coll.ID, t.rel)
	if err != nil {
		return nil, wrap(err)
	}
	if !found {
		if t.lead(f.pr) {
			return f.c.leadInfo(f.pr, t.norm), nil
		}
		return nil, webdav.NewHTTPError(http.StatusNotFound, chdrive.ErrNotFound)
	}
	return f.info(t, res), nil
}

func (f *fs) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	t, err := f.at(ctx, name)
	if err != nil {
		return nil, err
	}
	rc, _, err := f.c.store.Open(ctx, t.coll.ID, t.rel)
	if err != nil {
		return nil, wrap(err)
	}
	return rc, nil
}

// ReadDir lists the directory itself followed by its children (the
// library's contract: PROPFIND Depth 1 is the collection plus its
// members) or, recursive, everything below it. In the principal's own
// tree, the links below the directory are its children too — a linked
// drive, or a folder on the way to one — in place of anything of the
// collection's own at or below their names.
func (f *fs) ReadDir(ctx context.Context, name string, recursive bool) ([]webdav.FileInfo, error) {
	t, err := f.at(ctx, name)
	if err != nil {
		return nil, err
	}
	dir, found, err := f.c.store.Stat(ctx, t.coll.ID, t.rel)
	if err != nil {
		return nil, wrap(err)
	}
	var out []webdav.FileInfo
	switch {
	case found:
		out = append(out, *f.info(t, dir))
		if !dir.IsDir() {
			return out, nil
		}
	case t.lead(f.pr):
		out = append(out, *f.c.leadInfo(f.pr, t.norm))
	default:
		return nil, webdav.NewHTTPError(http.StatusNotFound, chdrive.ErrNotFound)
	}
	links := f.pr.coll.Links
	own := t.own() && len(links) > 0 && (t.norm != "" || strings.Trim(t.rel, "/") == "")
	if found {
		rows, err := f.c.store.List(ctx, t.coll.ID, chdrive.ListOpts{Path: t.rel, Recursive: recursive})
		if err != nil {
			return nil, wrap(err)
		}
		for _, r := range rows {
			if own {
				if _, _, _, hidden := links.Resolve(r.Path); hidden {
					continue // a link is in its place
				}
			}
			out = append(out, *f.info(t, r))
		}
	}
	if !own {
		return out, nil
	}
	for seg, isLink := range links.Under(t.norm) {
		p := chdrive.JoinPath(t.norm, seg)
		if !isLink {
			// Already listed as the collection's own folder, or shown here.
			if !hasPath(out, f.pr.prefix+"/"+p+"/") {
				out = append(out, *f.c.leadInfo(f.pr, p))
			}
			continue
		}
		lt, err := f.c.resolve(ctx, f.pr, f.pr.prefix+"/"+p)
		if err != nil {
			continue // linked to nothing (yet): not shown
		}
		root, ok, err := f.c.store.Stat(ctx, lt.coll.ID, "")
		if err != nil || !ok {
			continue
		}
		out = append(out, *f.info(lt, root))
	}
	return out, nil
}

func hasPath(infos []webdav.FileInfo, p string) bool {
	for _, fi := range infos {
		if fi.Path == p {
			return true
		}
	}
	return false
}

// Create is not reached: the head answers PUT itself (handler.go) so the
// body's Content-Length can size the stream. Kept for the interface.
func (f *fs) Create(ctx context.Context, name string, body io.ReadCloser, opts *webdav.CreateOptions) (*webdav.FileInfo, bool, error) {
	defer body.Close()
	return nil, false, webdav.NewHTTPError(http.StatusLengthRequired, errors.New("webdav: PUT is served by the head"))
}

func (f *fs) RemoveAll(ctx context.Context, name string, opts *webdav.RemoveAllOptions) error {
	t, err := f.at(ctx, name)
	if err != nil {
		return err
	}
	if t.own() && (t.rel == "" || t.rel == "/") {
		return webdav.NewHTTPError(http.StatusForbidden, errors.New("webdav: the mount root cannot be deleted"))
	}
	if t.held(f.pr) {
		return errHeld()
	}
	if err := f.check(t, chdrive.VerbDelete); err != nil {
		return err
	}
	// ConditionalMatch is the raw header value; the store parses it (a list
	// is legal) and applies it inside the delete's own transaction, so
	// there is no window between the check and the delete.
	var del chdrive.DeleteOpts
	if opts != nil {
		del.IfMatch, del.IfNoneMatch = string(opts.IfMatch), string(opts.IfNoneMatch)
	}
	_, err = f.c.store.Delete(ctx, t.coll.ID, t.rel, del)
	return wrap(err)
}

func (f *fs) Mkdir(ctx context.Context, name string) error {
	t, err := f.at(ctx, name)
	if err != nil {
		return err
	}
	if t.linkPoint() || (t.lead(f.pr) && !f.exists(ctx, t)) {
		// A linked drive, or a folder shown on the way to one: it's there.
		return webdav.NewHTTPError(http.StatusMethodNotAllowed, chdrive.ErrExists)
	}
	if err := f.check(t, chdrive.VerbCreate); err != nil {
		return err
	}
	_, err = f.c.store.Mkdir(ctx, t.coll.ID, t.rel)
	if errors.Is(err, chdrive.ErrExists) {
		// RFC 4918 §9.3.1: MKCOL on an existing resource is 405.
		return webdav.NewHTTPError(http.StatusMethodNotAllowed, err)
	}
	return wrap(err)
}

func (f *fs) Copy(ctx context.Context, name, dst string, opts *webdav.CopyOptions) (bool, error) {
	src, err := f.at(ctx, name)
	if err != nil {
		return false, err
	}
	to, err := f.dest(ctx, dst)
	if err != nil {
		return false, err
	}
	if to.held(f.pr) || to.lead(f.pr) {
		return false, errHeld()
	}
	// A copy only adds: the source is untouched, so only the destination
	// is judged.
	if err := f.check(to, chdrive.VerbMoveIn); err != nil {
		return false, err
	}
	overwrite := opts == nil || !opts.NoOverwrite
	existed := f.exists(ctx, to)
	if opts != nil && opts.NoRecursive {
		// Depth: 0 on a collection copies the collection without members.
		res, found, serr := f.c.store.Stat(ctx, src.coll.ID, src.rel)
		if serr != nil {
			return false, wrap(serr)
		}
		if !found {
			return false, webdav.NewHTTPError(http.StatusNotFound, chdrive.ErrNotFound)
		}
		if res.IsDir() {
			if existed && !overwrite {
				return false, webdav.NewHTTPError(http.StatusPreconditionFailed, chdrive.ErrExists)
			}
			if existed {
				if _, derr := f.c.store.Delete(ctx, to.coll.ID, to.rel, chdrive.DeleteOpts{}); derr != nil {
					return false, wrap(derr)
				}
			}
			_, merr := f.c.store.Mkdir(ctx, to.coll.ID, to.rel)
			return !existed, wrap(merr)
		}
	}
	if src.coll.ID == to.coll.ID {
		_, err = f.c.store.Copy(ctx, to.coll.ID, src.rel, to.rel, overwrite)
	} else {
		// Across a link: a copy into the destination's collection.
		_, err = f.c.store.CopyAcross(ctx, src.coll.ID, src.rel, to.coll.ID, to.rel, overwrite)
	}
	if err != nil {
		return false, wrap(err)
	}
	return !existed, nil
}

func (f *fs) Move(ctx context.Context, name, dst string, opts *webdav.MoveOptions) (bool, error) {
	from, err := f.at(ctx, name)
	if err != nil {
		return false, err
	}
	to, err := f.dest(ctx, dst)
	if err != nil {
		return false, err
	}
	if from.held(f.pr) || to.held(f.pr) || to.lead(f.pr) {
		return false, errHeld()
	}
	if from.coll.ID != to.coll.ID {
		return false, errCross()
	}
	// A move is two verbs: it removes from one subtree and adds to another,
	// and a policy may refuse either end.
	if err := f.check(from, chdrive.VerbMoveOut); err != nil {
		return false, err
	}
	if err := f.check(to, chdrive.VerbMoveIn); err != nil {
		return false, err
	}
	overwrite := opts == nil || !opts.NoOverwrite
	existed := f.exists(ctx, to)
	if _, err := f.c.store.Move(ctx, to.coll.ID, from.rel, to.rel, overwrite); err != nil {
		return false, wrap(err)
	}
	return !existed, nil
}

func (f *fs) exists(ctx context.Context, t target) bool {
	_, found, err := f.c.store.Stat(ctx, t.coll.ID, t.rel)
	return err == nil && found
}

// Compile-time interface check.
var _ webdav.FileSystem = (*fs)(nil)
