package filecas

// Streaming capability seams. The base Store contract is whole-blob []byte —
// right for FILES/ assets (small, LRU-cached), wrong for dataset artifacts
// that can run to gigabytes. Backends that can stream advertise it by
// implementing these OPTIONAL interfaces; callers go through the package
// helpers below, which discover the capability anywhere in the wrapper chain
// (the LRU cachedStore fronts every backend) and otherwise degrade to a
// buffered fallback over Put/Get. The bundled file backend implements all
// of them natively; the fleet S3 backend implements the reader pair and
// StreamPutter.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

// ErrTooLarge is returned by PutStream when the stream runs past its limit.
// Nothing is stored.
var ErrTooLarge = errors.New("filecas: stream exceeds limit")

// StreamPutter commits an UNTRUSTED stream whose hash is not known until
// EOF — the inverse of ReaderPutter, which is handed the hash up front by a
// client that already computed it (`txco apply`). A protocol inlet receiving
// a document from the network (a print job, an upload) cannot know the
// content address before the last byte, so the backend spools to a
// temporary, non-content-addressed location while hashing, and promotes to
// the content-addressed key only at EOF:
//
//	disk: temp file → os.Link into the sharded path
//	S3:   multipart upload to tmp/<id> → server-side CopyObject → delete tmp
//
// Contract: on ANY error (reader error, ctx cancelled, limit exceeded)
// nothing becomes visible under any hash and the temporary is removed.
// limit is the maximum accepted size in bytes; <= 0 means unlimited. A
// stream of exactly limit bytes is accepted; one byte more is ErrTooLarge.
// Identical content already present is a successful dedup, not an error.
type StreamPutter interface {
	PutStream(ctx context.Context, r io.Reader, limit int64) (hash string, size int64, err error)
}

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

// PutStream commits r into s and returns the content hash it computed,
// using the backend's native StreamPutter when it has one. The fallback
// spools to a local temp file while hashing, then hands the (now known)
// hash to PutReader — so it works over every backend, at the cost of one
// local write/read cycle the native implementations avoid. Callers receiving
// large payloads from the network MUST use this, never ReadAll + Put:
// receiving a blob must not require holding it in chassis memory.
func PutStream(ctx context.Context, s Store, r io.Reader, limit int64) (string, int64, error) {
	if sp, ok := capability[StreamPutter](s); ok {
		return sp.PutStream(ctx, r, limit)
	}
	tmp, err := os.CreateTemp("", "txco-filecas-spool-*")
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	hash, n, err := SpoolAndHash(ctx, tmp, r, limit)
	if err != nil {
		return "", 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	if err := PutReader(ctx, s, hash, tmp, n); err != nil {
		return "", 0, err
	}
	return hash, n, nil
}

// SpoolAndHash copies r into w while hashing, enforcing limit and ctx.
// Shared by the fallback above and by native StreamPutter implementations
// (the file backend spools to a temp file beside the blobs) so the limit
// and cancellation rules are written once. It reads at most limit+1 bytes:
// the extra byte is how "exactly limit" is told from "too large".
func SpoolAndHash(ctx context.Context, w io.Writer, r io.Reader, limit int64) (hash string, size int64, err error) {
	src := io.Reader(ctxReader{ctx: ctx, r: r})
	if limit > 0 {
		src = io.LimitReader(src, limit+1)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), src)
	if err != nil {
		return "", 0, err
	}
	if limit > 0 && n > limit {
		return "", 0, ErrTooLarge
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ctxReader fails the next Read once ctx is done, so a cancelled request
// (client gone, chassis draining) stops a copy loop between chunks instead
// of running the upload to completion.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
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
