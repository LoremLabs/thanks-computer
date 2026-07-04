package filecas

// Streaming capability seams. The base Store contract is whole-blob []byte —
// right for FILES/ assets (small, LRU-cached), wrong for dataset artifacts
// that can run to gigabytes. Backends that can stream advertise it by
// implementing these OPTIONAL interfaces; callers go through the package
// helpers below, which discover the capability anywhere in the wrapper chain
// (the LRU cachedStore fronts every backend) and otherwise degrade to a
// buffered fallback over Put/Get. The bundled file backend implements all
// three natively; the fleet S3 backend implements the reader pair.

import (
	"bytes"
	"context"
	"io"
)

// ReaderPutter streams content in. Implementations MUST hash while
// streaming and commit create-if-absent ONLY when sha256(stream)==hash —
// on mismatch they return ErrHashMismatch and store nothing (partial
// writes must never become visible). size is the exact expected byte
// count when the caller knows it (Content-Length), or -1; backends that
// need sizing up front (multipart uploads) may require it.
type ReaderPutter interface {
	PutReader(ctx context.Context, hash string, r io.Reader, size int64) error
}

// ReaderGetter streams content out. Returns the reader, the blob size,
// and ErrNotFound when absent. The caller owns closing the reader.
type ReaderGetter interface {
	GetReader(ctx context.Context, hash string) (io.ReadCloser, int64, error)
}

// PathProvider exposes a blob's local filesystem path, for zero-copy
// consumers (opening a dataset artifact read-only in place rather than
// materialising a second multi-GB copy). ok=false when the hash is absent
// or the backend has no local files (S3). The returned path is
// content-addressed and therefore immutable — callers MUST NOT write to it.
type PathProvider interface {
	BlobPath(hash string) (path string, ok bool)
}

// unwrapper lets the capability discovery below see through decorators
// (the LRU cachedStore). Internal: decorators in this package implement it.
type unwrapper interface {
	unwrap() Store
}

// capability walks the decorator chain looking for capability C.
func capability[C any](s Store) (C, bool) {
	for s != nil {
		if c, ok := s.(C); ok {
			return c, true
		}
		u, ok := s.(unwrapper)
		if !ok {
			break
		}
		s = u.unwrap()
	}
	var zero C
	return zero, false
}

// PutReader streams r into s under hash, using the backend's native
// streaming when available and a buffered fallback (ReadAll → Put, which
// verifies the hash) otherwise. The fallback holds the whole blob in
// memory — acceptable for backends that never advertised streaming, since
// their Put would have buffered anyway.
func PutReader(ctx context.Context, s Store, hash string, r io.Reader, size int64) error {
	if rp, ok := capability[ReaderPutter](s); ok {
		return rp.PutReader(ctx, hash, r, size)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return s.Put(ctx, hash, data)
}

// GetReader streams the blob for hash out of s, via the backend's native
// reader when available, else a buffered fallback over Get (which may be
// served from the LRU).
func GetReader(ctx context.Context, s Store, hash string) (io.ReadCloser, int64, error) {
	if rg, ok := capability[ReaderGetter](s); ok {
		return rg.GetReader(ctx, hash)
	}
	data, err := s.Get(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// BlobPath reports the local filesystem path for hash when the backend has
// one (the bundled file backend). No fallback — absence means "materialise
// a copy yourself via GetReader".
func BlobPath(s Store, hash string) (string, bool) {
	pp, ok := capability[PathProvider](s)
	if !ok {
		return "", false
	}
	return pp.BlobPath(hash)
}
