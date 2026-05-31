package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GetDNSRender fetches the rendered authoritative zone-file for the
// target tenant from GET /v1/tenants/{tenant}/dns/render. The response
// is text/plain (a zone-file), not JSON. `zone` optionally limits the
// output to a single origin; empty renders all the tenant's zones.
func (c *Client) GetDNSRender(ctx context.Context, zone string) (string, error) {
	suffix := "/dns/render"
	if z := strings.TrimSpace(zone); z != "" {
		suffix += "?zone=" + url.QueryEscape(z)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scopedURL(suffix), nil)
	if err != nil {
		return "", err
	}
	if err := c.applyAuth(req, nil); err != nil {
		return "", err
	}
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", decodeError(resp)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read dns render: %w", err)
	}
	return string(b), nil
}
