package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Browser-auth client methods. Mirrors the wire shapes in
// chassis/server/admin/browserauth.go.

// BootstrapBrowserAuth mints a single-use exchange token. The
// returned URL is what the user opens to complete login in their
// browser; the token is embedded in the URL's hash fragment.
type BootstrapBrowserResponse struct {
	Token            string `json:"token"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
	URL              string `json:"url"`
}

// SessionRecord mirrors the chassis's sessionRecord; one entry in the
// list-sessions response or the read side of a future "manage
// sessions" view.
type SessionRecord struct {
	SessionID  string  `json:"session_id"`
	ActorID    string  `json:"actor_id"`
	TenantID   string  `json:"tenant_id"`
	UA         string  `json:"ua,omitempty"`
	IP         string  `json:"ip,omitempty"`
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  string  `json:"expires_at"`
	LastSeenAt string  `json:"last_seen_at"`
	RevokedAt  *string `json:"revoked_at,omitempty"`
	RevokedBy  string  `json:"revoked_by,omitempty"`
}

type listBrowserSessionsResponse struct {
	Sessions []SessionRecord `json:"sessions"`
}

type bootstrapBrowserRequest struct {
	Label      string `json:"label,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// BootstrapBrowserAuth signs a POST to /auth/browser/bootstrap and
// returns the mint response. The CLI then `open`s the URL (or
// prints it for the user to paste into a browser).
func (c *Client) BootstrapBrowserAuth(ctx context.Context, label string) (*BootstrapBrowserResponse, error) {
	body, err := json.Marshal(bootstrapBrowserRequest{Label: label})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/auth/browser/bootstrap")
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out BootstrapBrowserResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode bootstrap browser response: %w", err)
	}
	return &out, nil
}

// ListBrowserSessions returns every session for the target tenant,
// newest first.
func (c *Client) ListBrowserSessions(ctx context.Context) ([]SessionRecord, error) {
	endpoint := c.scopedURL("/auth/sessions")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
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
	var out listBrowserSessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list browser sessions: %w", err)
	}
	return out.Sessions, nil
}

// RevokeBrowserSession revokes one session by id. Idempotent at the
// server.
func (c *Client) RevokeBrowserSession(ctx context.Context, sessionID string) error {
	endpoint := c.scopedURL("/auth/sessions/" + sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
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
