package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// HasBlob reports whether the chassis CAS already holds hash, so apply can
// skip re-streaming an unchanged dataset artifact (the "have" probe).
func (c *Client) HasBlob(ctx context.Context, hash string) (bool, error) {
	endpoint := c.scopedURL("/blobs/sha256/" + url.PathEscape(hash))
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return false, err
	}
	if err := c.applyAuth(req, nil); err != nil {
		return false, err
	}
	resp, err := c.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, decodeError(resp)
	}
}

// ErrNoMissingBlobs reports a chassis that predates POST /blobs/missing (the
// batch probe). The same release taught it COMPUTES/ rows, so callers also
// read it as "this chassis doesn't keep compute source".
var ErrNoMissingBlobs = errors.New("chassis has no batch blob probe")

// MaxMissingBlobsBatch is the most hashes one POST /blobs/missing carries.
const MaxMissingBlobsBatch = 1000

// MissingBlobs returns which of hashes the chassis CAS does not hold — one
// request per MaxMissingBlobsBatch hashes instead of a HEAD each. It returns
// ErrNoMissingBlobs from an older chassis (404), so the caller can fall back
// to HasBlob. An empty list is a valid probe of the endpoint itself.
func (c *Client) MissingBlobs(ctx context.Context, hashes []string) ([]string, error) {
	missing := []string{}
	for start := 0; start == 0 || start < len(hashes); start += MaxMissingBlobsBatch {
		chunk, err := c.missingBlobsBatch(ctx, hashes[start:min(start+MaxMissingBlobsBatch, len(hashes))])
		if err != nil {
			return nil, err
		}
		missing = append(missing, chunk...)
	}
	return missing, nil
}

func (c *Client) missingBlobsBatch(ctx context.Context, hashes []string) ([]string, error) {
	body, err := json.Marshal(map[string][]string{"hashes": hashes})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/blobs/missing")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.applyAuth(req, body); err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil, ErrNoMissingBlobs
	default:
		return nil, decodeError(resp)
	}
	var out struct {
		Missing []string `json:"missing"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode blobs/missing: %w", err)
	}
	return out.Missing, nil
}

// GetBlob streams a blob out of the chassis CAS. The caller owns closing
// the reader — and, because the transport is a plain body stream, SHOULD
// verify the received bytes hash to `hash` before trusting them (the pull
// path does).
func (c *Client) GetBlob(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	endpoint := c.scopedURL("/blobs/sha256/" + url.PathEscape(hash))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	if err := c.applyAuth(req, nil); err != nil {
		return nil, 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, 0, decodeError(resp)
	}
	return resp.Body, resp.ContentLength, nil
}

// PutBlob streams size bytes from r into the chassis CAS under hash
// (lowercase sha256 hex of the bytes, which the caller has already computed
// by streaming the file — see hashFileStreaming). The body is never held in
// memory; Content-Digest is pre-set from the known hash so the request
// signature covers it without the signer needing the bytes. The server
// re-hashes the stream and refuses a mismatch, so a corrupt read or
// truncated transfer cannot poison the CAS. Idempotent.
func (c *Client) PutBlob(ctx context.Context, hash string, r io.Reader, size int64) error {
	raw, err := hex.DecodeString(hash)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("putblob: malformed sha256 hex %q", hash)
	}
	endpoint := c.scopedURL("/blobs/sha256/" + url.PathEscape(hash))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	// Pre-set Content-Digest so applyAuth signs over the streamed body's
	// real digest (computeContentDigest honors a caller-set value).
	req.Header.Set("Content-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(raw)+":")
	if err := c.applyAuth(req, nil); err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	return nil
}
