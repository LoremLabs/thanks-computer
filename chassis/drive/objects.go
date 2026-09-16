package drive

import (
	"context"
	"fmt"
	"io"
	"time"
)

// ObjectInfo is what an object store knows about a stored object without
// reading it.
type ObjectInfo struct {
	Size    int64
	ModTime time.Time
	// ContentType is what the backend recorded at Put, when it records one
	// (S3 does; the file backend does not). The index is authoritative.
	ContentType string
}

// ObjectStore holds resource bytes under keys the Store mints:
// `<tenant>/<collection_id>/<resource_id>/<version_id>` — "/"-separated,
// every segment `[A-Za-z0-9._-]`. A backend adds its own root (a directory,
// an S3 prefix) and must never let a key escape it. Objects are written once
// and never rewritten: a new version is a new key, and the sweeper deletes
// keys no live index row references.
//
// The interface is deliberately minimal (put / get / stat / list / delete):
// it is what a mutable document store needs from ordinary object storage,
// and what a fleet overlay can implement over S3 in one file.
type ObjectStore interface {
	// Put streams r to key. size is the caller's declared byte count when
	// known (-1 otherwise) — a hint a backend may use to pick a single-part
	// upload; the Store verifies the count itself. A Put must be atomic: a
	// reader never observes a partial object, and a failed Put leaves no
	// object at key.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error

	// Get opens the object for reading. ErrNotFound if absent.
	Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)

	// Stat reports the object's metadata. ErrNotFound if absent.
	Stat(ctx context.Context, key string) (ObjectInfo, error)

	// List walks every object under prefix ("" for all), calling fn for each;
	// fn's error stops the walk and is returned. Pagination is the backend's.
	List(ctx context.Context, prefix string, fn func(key string, info ObjectInfo) error) error

	// Delete removes key. Absent key is not an error (idempotent).
	Delete(ctx context.Context, key string) error

	// Name is the backend identity ("file", "s3").
	Name() string
}

// ObjectCopier is the optional server-side copy capability (S3 CopyObject,
// a hard link). The Store type-asserts it; a backend without it is copied
// through Get → Put.
type ObjectCopier interface {
	Copy(ctx context.Context, from, to string) error
}

// copyObject copies from → to server-side when the backend can, else by
// streaming.
func copyObject(ctx context.Context, st ObjectStore, from, to string) error {
	if c, ok := st.(ObjectCopier); ok {
		return c.Copy(ctx, from, to)
	}
	rc, info, err := st.Get(ctx, from)
	if err != nil {
		return err
	}
	defer rc.Close()
	return st.Put(ctx, to, rc, info.Size, info.ContentType)
}

// ObjectsConfig carries backend-selecting options for the OBJECT store
// (--drive-objects). The bundled "file" backend uses FileDir; an overlay's
// S3 backend reads its own environment.
type ObjectsConfig struct {
	// FileDir is the bundled file backend's root (--drive-objects-file-dir).
	FileDir string
}

// ObjectsConstructor builds an ObjectStore from resolved config.
type ObjectsConstructor func(ObjectsConfig) (ObjectStore, error)

var objectBackends = map[string]ObjectsConstructor{}

// RegisterObjects adds an object backend constructor. Called from a backend
// package's init(); the chassis activates a backend with a blank import.
func RegisterObjects(name string, c ObjectsConstructor) {
	objectBackends[name] = c
}

// OpenObjects constructs the named object backend. Unknown name is a startup
// error listing what is available.
func OpenObjects(name string, cfg ObjectsConfig) (ObjectStore, error) {
	c, ok := objectBackends[name]
	if !ok {
		avail := make([]string, 0, len(objectBackends))
		for k := range objectBackends {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("drive: unknown object store %q (available: %v)", name, avail)
	}
	return c(cfg)
}

// ValidKeySegment reports whether one "/"-separated object-key segment is
// safe for any backend: `[A-Za-z0-9._-]`, non-empty, not only dots (the
// filesystem traversal entries). Keys are built from ids the Store mints,
// so this is a reject check, not a sanitizer — a bad segment is a bug.
func ValidKeySegment(seg string) bool {
	if seg == "" {
		return false
	}
	dots := 0
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		case c == '.':
			dots++
		default:
			return false
		}
	}
	return dots != len(seg)
}
