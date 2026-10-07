package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Allowance is one tenant-defined fuel budget and its current window
// (GET/PUT /v1/tenants/{t}/allowances/{name}). Remaining is nil for an
// allowance that was entered but never defined: metered, not limited.
type Allowance struct {
	Name      string `json:"name"`
	Defined   bool   `json:"defined"`
	Fuel      int64  `json:"fuel,omitempty"`
	Per       string `json:"per"`
	Used      int64  `json:"used"`
	Remaining *int64 `json:"remaining,omitempty"`
	ResetsAt  string `json:"resets_at,omitempty"`
}

// AllowanceList is one page of GET /v1/tenants/{t}/allowances.
type AllowanceList struct {
	Allowances []Allowance `json:"allowances"`
	Next       string      `json:"next,omitempty"`
	Count      int         `json:"count"`
}

// ListAllowances returns a page of the tenant's allowances after the cursor.
func (c *Client) ListAllowances(ctx context.Context, after string, limit int) (*AllowanceList, error) {
	endpoint := c.scopedURL("/allowances")
	q := url.Values{}
	if after != "" {
		q.Set("after", after)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if enc := q.Encode(); enc != "" {
		endpoint += "?" + enc
	}
	var out AllowanceList
	if err := c.allowanceCall(ctx, http.MethodGet, endpoint, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAllowance reads one allowance's current window.
func (c *Client) GetAllowance(ctx context.Context, name string) (*Allowance, error) {
	var out Allowance
	if err := c.allowanceCall(ctx, http.MethodGet, c.scopedURL("/allowances/"+url.PathEscape(name)), nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetAllowance creates or replaces an allowance: fuel per hour, day or month
// (per "" = the server's default, day).
func (c *Client) SetAllowance(ctx context.Context, name string, fuel int64, per string) (*Allowance, error) {
	body, err := json.Marshal(struct {
		Fuel int64  `json:"fuel"`
		Per  string `json:"per,omitempty"`
	}{fuel, per})
	if err != nil {
		return nil, err
	}
	var out Allowance
	if err := c.allowanceCall(ctx, http.MethodPut, c.scopedURL("/allowances/"+url.PathEscape(name)), body, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteAllowance removes an allowance's definition.
func (c *Client) DeleteAllowance(ctx context.Context, name string) error {
	return c.allowanceCall(ctx, http.MethodDelete, c.scopedURL("/allowances/"+url.PathEscape(name)), nil, http.StatusNoContent, nil)
}

func (c *Client) allowanceCall(ctx context.Context, method, endpoint string, body []byte, want int, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.applyAuth(req, body); err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		return decodeError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode allowance response: %w", err)
	}
	return nil
}
