package client

import (
	"context"
	"io"
	"net/http"
)

// OpenStream issues a signed GET to a tenant-scoped endpoint and returns the
// raw response body for streaming (e.g. an SSE feed). The caller MUST Close the
// returned body.
//
// Unlike DoScoped it imposes NO client-level timeout — the stream is long-lived
// and cancelled via ctx, not a deadline. A non-2xx status is returned as a
// decoded server error.
func (c *Client) OpenStream(ctx context.Context, suffix string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scopedURL(suffix), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	if err := c.applyAuth(req, nil); err != nil {
		return nil, err
	}
	// A dedicated client with no timeout: c.http caps requests at 30s, which
	// would sever an idle SSE feed.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, prettifyNetworkError(err, c.target.Addr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, decodeError(resp)
	}
	return resp.Body, nil
}
