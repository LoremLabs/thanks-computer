package client

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
)

// HeadCompute reports whether a compute artifact is already present, so apply
// can skip re-uploading unchanged modules.
func (c *Client) HeadCompute(ctx context.Context, alg, digest string) (bool, error) {
	endpoint := c.scopedURL("/computes/" + url.PathEscape(alg) + "/" + url.PathEscape(digest))
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

// PutCompute uploads a content-addressed compute module. The server verifies
// sha256(body)==digest; the upload is idempotent. engine (e.g. "wazero") is
// recorded in the artifact manifest.
func (c *Client) PutCompute(ctx context.Context, alg, digest, engine string, wasm []byte) error {
	suffix := "/computes/" + url.PathEscape(alg) + "/" + url.PathEscape(digest)
	if engine != "" {
		suffix += "?engine=" + url.QueryEscape(engine)
	}
	endpoint := c.scopedURL(suffix)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(wasm))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/wasm")
	if err := c.applyAuth(req, wasm); err != nil {
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
