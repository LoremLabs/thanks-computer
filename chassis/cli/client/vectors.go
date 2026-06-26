package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// VectorCollection is one collection's pin + item count (`txco vector ls`).
type VectorCollection struct {
	Name           string `json:"name"`
	EmbeddingModel string `json:"embedding_model,omitempty"`
	Dimensions     int    `json:"dimensions"`
	Metric         string `json:"metric"`
	Count          int    `json:"count"`
}

// VectorCollectionDetail adds the item IDs (`txco vector show`/`diff`).
type VectorCollectionDetail struct {
	VectorCollection
	IDs []string `json:"ids"`
}

// VectorListCollections: GET /vectors
func (c *Client) VectorListCollections(ctx context.Context) ([]VectorCollection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scopedURL("/vectors"), nil)
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
		Collections []VectorCollection `json:"collections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode vector collections: %w", err)
	}
	return out.Collections, nil
}

// VectorGetCollection: GET /vectors/{name}
func (c *Client) VectorGetCollection(ctx context.Context, name string) (*VectorCollectionDetail, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.scopedURL("/vectors/"+url.PathEscape(name)), nil)
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
	var out VectorCollectionDetail
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode vector collection: %w", err)
	}
	return &out, nil
}

// VectorDropCollection: DELETE /vectors/{name}. Returns the number of items removed.
func (c *Client) VectorDropCollection(ctx context.Context, name string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.scopedURL("/vectors/"+url.PathEscape(name)), nil)
	if err != nil {
		return 0, err
	}
	if err := c.applyAuth(req, nil); err != nil {
		return 0, err
	}
	resp, err := c.do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, decodeError(resp)
	}
	var out struct {
		RemovedItems int `json:"removed_items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode drop collection: %w", err)
	}
	return out.RemovedItems, nil
}
