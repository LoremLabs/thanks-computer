package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// --- render -----------------------------------------------------------

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

// --- zones + records --------------------------------------------------

// DNSZoneInfo mirrors the admin zone DTO.
type DNSZoneInfo struct {
	Origin     string `json:"origin"`
	Mode       string `json:"mode"`
	MName      string `json:"mname"`
	RName      string `json:"rname"`
	DefaultTTL int    `json:"default_ttl"`
	CreatedAt  string `json:"created_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
}

// CreateZoneResult is the create-zone response (zone + delegation hint).
type CreateZoneResult struct {
	Zone        DNSZoneInfo `json:"zone"`
	Nameservers []string    `json:"nameservers"`
	Delegation  string      `json:"delegation"`
}

// DNSRecordInfo mirrors the admin record DTO.
type DNSRecordInfo struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	TTL   *int64 `json:"ttl,omitempty"`
	Rdata string `json:"rdata"`
}

// CreateZone registers a delegated zone. mode is "" (pattern) or "manual".
func (c *Client) CreateZone(ctx context.Context, origin, mode string) (*CreateZoneResult, error) {
	body, err := json.Marshal(map[string]string{"origin": origin, "mode": mode})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.scopedURL("/dns/zones"), bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusCreated {
		return nil, decodeError(resp)
	}
	var out CreateZoneResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode create zone: %w", err)
	}
	return &out, nil
}

// ListZones returns the tenant's active zones.
func (c *Client) ListZones(ctx context.Context) ([]DNSZoneInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scopedURL("/dns/zones"), nil)
	if err != nil {
		return nil, err
	}
	if err := c.applyAuth(req, nil); err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out struct {
		Zones []DNSZoneInfo `json:"zones"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list zones: %w", err)
	}
	return out.Zones, nil
}

// RevokeZone soft-revokes a delegated zone by origin.
func (c *Client) RevokeZone(ctx context.Context, origin string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.scopedURL("/dns/zones/"+url.PathEscape(origin)), nil)
	if err != nil {
		return err
	}
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

// CreateRecord adds an override/extra record under a zone. ttl<0 means
// "inherit the zone default".
func (c *Client) CreateRecord(ctx context.Context, origin, name, rtype string, ttl int64, rdata string) error {
	payload := map[string]any{"name": name, "type": rtype, "rdata": rdata}
	if ttl >= 0 {
		payload["ttl"] = ttl
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.scopedURL("/dns/zones/"+url.PathEscape(origin)+"/records"), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.applyAuth(req, body); err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return decodeError(resp)
	}
	return nil
}

// ListRecords returns the active override records under a zone.
func (c *Client) ListRecords(ctx context.Context, origin string) ([]DNSRecordInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.scopedURL("/dns/zones/"+url.PathEscape(origin)+"/records"), nil)
	if err != nil {
		return nil, err
	}
	if err := c.applyAuth(req, nil); err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out struct {
		Records []DNSRecordInfo `json:"records"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list records: %w", err)
	}
	return out.Records, nil
}

// RevokeRecord soft-revokes records matching (name, type) under a zone.
func (c *Client) RevokeRecord(ctx context.Context, origin, name, rtype string) error {
	suffix := "/dns/zones/" + url.PathEscape(origin) + "/records" +
		"?name=" + url.QueryEscape(name) + "&type=" + url.QueryEscape(rtype)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.scopedURL(suffix), nil)
	if err != nil {
		return err
	}
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
