package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// StackBlob is one name a stack's BLOBS/ tree seeded, as reported by
// GET /stacks/{name}/blobs: the live sha, the sha the tree last shipped, and
// whether a runtime put moved it since (drift).
type StackBlob struct {
	Name        string `json:"name"`
	SHA256      string `json:"sha256"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type,omitempty"`
	Filename    string `json:"filename,omitempty"`
	SeededSHA   string `json:"seeded_sha"`
	Drifted     bool   `json:"drifted"`
	UpdatedAt   string `json:"updated_at"`
}

// ListStackBlobs: GET /stacks/{name}/blobs — the names the stack's BLOBS/
// tree seeded, with drift. `txco data apply` refuses to push over drift
// unless forced; `txco data pull` materialises it into the tree.
func (c *Client) ListStackBlobs(ctx context.Context, stack string) ([]StackBlob, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scopedURL(stackPath(stack, "/blobs")), nil)
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
		Names []StackBlob `json:"names"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode stack blobs: %w", err)
	}
	return out.Names, nil
}
