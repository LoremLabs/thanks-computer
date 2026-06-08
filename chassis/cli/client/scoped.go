package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// DoScoped issues a signed request to a tenant-scoped endpoint living under
// /v1/tenants/{tenant}<suffix> (suffix is slash-prefixed, e.g. "/credit/balance").
// body is JSON-encoded when non-nil; the 2xx response is decoded into out when
// non-nil. A non-2xx status is returned as a decoded server error.
//
// It's the generic primitive overlay-registered CLI verbs reuse so they never
// reimplement signing or tenant-scoping — the same applyAuth/scopedURL/do path
// the built-in client methods use.
func (c *Client) DoScoped(ctx context.Context, method, suffix string, body, out any) error {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}
	var rdr io.Reader
	if raw != nil {
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.scopedURL(suffix), rdr)
	if err != nil {
		return err
	}
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.applyAuth(req, raw); err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeError(resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
