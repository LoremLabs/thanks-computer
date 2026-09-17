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

// fs adapts one principal's collection to the library's FileSystem. Names
// arrive as full URL paths (the library has no prefix), so every name is
// stripped on the way in and every FileInfo.Path is rebuilt on the way
// out; a COPY/MOVE Destination outside the prefix is refused. Every error
// is a webdav.HTTPError — the library answers 500 for anything else.
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

func (f *fs) info(res chdrive.Resource) *webdav.FileInfo {
	fi := &webdav.FileInfo{
		Path:    f.c.href(res),
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

// dest validates a COPY/MOVE destination: inside the mount, on the same
// collection by construction.
func (f *fs) dest(p string) (string, error) {
	if p != f.c.prefix && !strings.HasPrefix(p, f.c.prefix+"/") {
		return "", webdav.NewHTTPError(http.StatusForbidden, errOutsidePrefix)
	}
	return f.c.rel(p), nil
}

func (f *fs) Stat(ctx context.Context, name string) (*webdav.FileInfo, error) {
	res, found, err := f.c.store.Stat(ctx, f.pr.coll.ID, f.c.rel(name))
	if err != nil {
		return nil, wrap(err)
	}
	if !found {
		return nil, webdav.NewHTTPError(http.StatusNotFound, chdrive.ErrNotFound)
	}
	return f.info(res), nil
}

func (f *fs) Open(ctx context.Context, name string) (io.ReadCloser, error) {
	rc, _, err := f.c.store.Open(ctx, f.pr.coll.ID, f.c.rel(name))
	if err != nil {
		return nil, wrap(err)
	}
	return rc, nil
}

// ReadDir lists the directory itself followed by its children (the
// library's contract: PROPFIND Depth 1 is the collection plus its
// members) or, recursive, everything below it.
func (f *fs) ReadDir(ctx context.Context, name string, recursive bool) ([]webdav.FileInfo, error) {
	rel := f.c.rel(name)
	dir, found, err := f.c.store.Stat(ctx, f.pr.coll.ID, rel)
	if err != nil {
		return nil, wrap(err)
	}
	if !found {
		return nil, webdav.NewHTTPError(http.StatusNotFound, chdrive.ErrNotFound)
	}
	out := []webdav.FileInfo{*f.info(dir)}
	if !dir.IsDir() {
		return out, nil
	}
	rows, err := f.c.store.List(ctx, f.pr.coll.ID, chdrive.ListOpts{Path: rel, Recursive: recursive})
	if err != nil {
		return nil, wrap(err)
	}
	for _, r := range rows {
		out = append(out, *f.info(r))
	}
	return out, nil
}

// Create is not reached: the head answers PUT itself (handler.go) so the
// body's Content-Length can size the stream. Kept for the interface.
func (f *fs) Create(ctx context.Context, name string, body io.ReadCloser, opts *webdav.CreateOptions) (*webdav.FileInfo, bool, error) {
	defer body.Close()
	return nil, false, webdav.NewHTTPError(http.StatusLengthRequired, errors.New("webdav: PUT is served by the head"))
}

func (f *fs) RemoveAll(ctx context.Context, name string, opts *webdav.RemoveAllOptions) error {
	rel := f.c.rel(name)
	if rel == "" || rel == "/" {
		return webdav.NewHTTPError(http.StatusForbidden, errors.New("webdav: the mount root cannot be deleted"))
	}
	if err := f.check(rel, chdrive.VerbDelete); err != nil {
		return err
	}
	var ifMatch string
	if opts != nil {
		ifMatch = unquoteETag(string(opts.IfMatch))
		if opts.IfNoneMatch.IsSet() {
			// If-None-Match on DELETE: refuse when the current etag matches.
			res, found, err := f.c.store.Stat(ctx, f.pr.coll.ID, rel)
			if err != nil {
				return wrap(err)
			}
			if found && (opts.IfNoneMatch.IsWildcard() || unquoteETag(string(opts.IfNoneMatch)) == res.ETag) {
				return webdav.NewHTTPError(http.StatusPreconditionFailed, chdrive.ErrPrecondition)
			}
		}
	}
	_, err := f.c.store.Delete(ctx, f.pr.coll.ID, rel, chdrive.DeleteOpts{IfMatch: ifMatch})
	return wrap(err)
}

func (f *fs) Mkdir(ctx context.Context, name string) error {
	rel := f.c.rel(name)
	if err := f.check(rel, chdrive.VerbCreate); err != nil {
		return err
	}
	_, err := f.c.store.Mkdir(ctx, f.pr.coll.ID, rel)
	if errors.Is(err, chdrive.ErrExists) {
		// RFC 4918 §9.3.1: MKCOL on an existing resource is 405.
		return webdav.NewHTTPError(http.StatusMethodNotAllowed, err)
	}
	return wrap(err)
}

func (f *fs) Copy(ctx context.Context, name, dst string, opts *webdav.CopyOptions) (bool, error) {
	to, err := f.dest(dst)
	if err != nil {
		return false, err
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
		src, found, serr := f.c.store.Stat(ctx, f.pr.coll.ID, f.c.rel(name))
		if serr != nil {
			return false, wrap(serr)
		}
		if !found {
			return false, webdav.NewHTTPError(http.StatusNotFound, chdrive.ErrNotFound)
		}
		if src.IsDir() {
			if existed && !overwrite {
				return false, webdav.NewHTTPError(http.StatusPreconditionFailed, chdrive.ErrExists)
			}
			if existed {
				if _, derr := f.c.store.Delete(ctx, f.pr.coll.ID, to, chdrive.DeleteOpts{}); derr != nil {
					return false, wrap(derr)
				}
			}
			_, merr := f.c.store.Mkdir(ctx, f.pr.coll.ID, to)
			return !existed, wrap(merr)
		}
	}
	if _, err := f.c.store.Copy(ctx, f.pr.coll.ID, f.c.rel(name), to, overwrite); err != nil {
		return false, wrap(err)
	}
	return !existed, nil
}

func (f *fs) Move(ctx context.Context, name, dst string, opts *webdav.MoveOptions) (bool, error) {
	to, err := f.dest(dst)
	if err != nil {
		return false, err
	}
	// A move is two verbs: it removes from one subtree and adds to another,
	// and a policy may refuse either end.
	from := f.c.rel(name)
	if err := f.check(from, chdrive.VerbMoveOut); err != nil {
		return false, err
	}
	if err := f.check(to, chdrive.VerbMoveIn); err != nil {
		return false, err
	}
	overwrite := opts == nil || !opts.NoOverwrite
	existed := f.exists(ctx, to)
	if _, err := f.c.store.Move(ctx, f.pr.coll.ID, f.c.rel(name), to, overwrite); err != nil {
		return false, wrap(err)
	}
	return !existed, nil
}

func (f *fs) exists(ctx context.Context, rel string) bool {
	_, found, err := f.c.store.Stat(ctx, f.pr.coll.ID, rel)
	return err == nil && found
}

// Compile-time interface check.
var _ webdav.FileSystem = (*fs)(nil)
