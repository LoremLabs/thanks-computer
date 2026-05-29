package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Wire-shape mirrors of chassis/server/admin/stacks.go. Kept duplicated
// rather than imported so the CLI doesn't pull the server package.

type StackRecord struct {
	Name          string `json:"name"`
	ActiveVersion *int64 `json:"active_version,omitempty"`
	CreatedAt     string `json:"created_at"`
}

type VersionRecord struct {
	VersionNumber int64   `json:"version_number"`
	Status        string  `json:"status"`
	ParentVersion *int64  `json:"parent_version_number,omitempty"`
	CreatedBy     string  `json:"created_by"`
	CreatedAt     string  `json:"created_at"`
	ActivatedAt   *string `json:"activated_at,omitempty"`
	ManifestHash  string  `json:"manifest_hash"`
	IsActive      bool    `json:"is_active"`
}

type StackFile struct {
	Path        string `json:"path"`
	Content     string `json:"content,omitempty"`
	ContentHash string `json:"content_hash"`
}

type VersionDetail struct {
	VersionRecord
	Files []StackFile `json:"files"`
}

type listStacksResp struct {
	Stacks []StackRecord `json:"stacks"`
}

type listVersionsResp struct {
	Versions []VersionRecord `json:"versions"`
}

type createDraftReq struct {
	From string `json:"from,omitempty"`
}

type createDraftResp struct {
	VersionNumber int64 `json:"version_number"`
}

type putFilesReq struct {
	Files []StackFile `json:"files"`
}

type PutFilesResponse struct {
	ManifestHash string `json:"manifest_hash"`
}

type activateReq struct {
	VersionNumber int64 `json:"version_number"`
}

type ActivateResponse struct {
	PriorVersionNumber *int64 `json:"prior_version_number,omitempty"`
	VersionNumber      int64  `json:"version_number"`
	StructuredURL      string `json:"structured_url,omitempty"`
}

type ValidateError struct {
	Path string `json:"path"`
	Err  string `json:"err"`
}

type ValidateResponse struct {
	OK      bool            `json:"ok"`
	Errors  []ValidateError `json:"errors,omitempty"`
	Checked int             `json:"checked"`
}

type DiffEntry struct {
	Path     string `json:"path"`
	Change   string `json:"change"`
	FromHash string `json:"from_hash,omitempty"`
	ToHash   string `json:"to_hash,omitempty"`
}

type DiffResponse struct {
	V1      int64       `json:"v1"`
	V2      int64       `json:"v2"`
	Equal   bool        `json:"equal"`
	Entries []DiffEntry `json:"entries,omitempty"`
}

type patchFileReq struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	BaseHash string `json:"base_hash"`
}

type PatchFileResponse struct {
	Path         string `json:"path"`
	ContentHash  string `json:"content_hash"`
	ManifestHash string `json:"manifest_hash"`
}

type deleteFileReq struct {
	Path     string `json:"path"`
	BaseHash string `json:"base_hash"`
}

type DeleteFileResponse struct {
	Path         string `json:"path"`
	Deleted      bool   `json:"deleted"`
	ManifestHash string `json:"manifest_hash"`
}

// stackPath builds the suffix under /v1/tenants/{t}/ for stack-level
// endpoints. Stack names can contain slashes (e.g. "website/canary");
// each segment is percent-encoded but slashes between segments are
// preserved so the server's `{name:.+}` matcher sees the right path.
func stackPath(name string, tail string) string {
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "/stacks/" + strings.Join(parts, "/") + tail
}

// ListStacks: GET /stacks
func (c *Client) ListStacks(ctx context.Context) ([]StackRecord, error) {
	endpoint := c.scopedURL("/stacks")
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
	var out listStacksResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list stacks: %w", err)
	}
	return out.Stacks, nil
}

// GetStack: GET /stacks/{name}
func (c *Client) GetStack(ctx context.Context, name string) (*StackRecord, error) {
	endpoint := c.scopedURL(stackPath(name, ""))
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
	var out StackRecord
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode get stack: %w", err)
	}
	return &out, nil
}

// ListVersions: GET /stacks/{name}/versions
func (c *Client) ListVersions(ctx context.Context, name string) ([]VersionRecord, error) {
	endpoint := c.scopedURL(stackPath(name, "/versions"))
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
	var out listVersionsResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list versions: %w", err)
	}
	return out.Versions, nil
}

// GetVersion: GET /stacks/{name}/versions/{n}?include=content
func (c *Client) GetVersion(ctx context.Context, name string, versionNumber int64, includeContent bool) (*VersionDetail, error) {
	suffix := stackPath(name, "/versions/"+strconv.FormatInt(versionNumber, 10))
	endpoint := c.scopedURL(suffix)
	if includeContent {
		endpoint += "?include=content"
	}
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
	var out VersionDetail
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode get version: %w", err)
	}
	return &out, nil
}

// CreateDraft: POST /stacks/{name}/draft
// from: "active" to clone the currently-active version, "<N>" to clone a
// specific version_number, or "" for an empty draft.
func (c *Client) CreateDraft(ctx context.Context, name, from string) (int64, error) {
	body, err := json.Marshal(createDraftReq{From: from})
	if err != nil {
		return 0, err
	}
	endpoint := c.scopedURL(stackPath(name, "/draft"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.applyAuth(req, body); err != nil {
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
	var out createDraftResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode create draft: %w", err)
	}
	return out.VersionNumber, nil
}

// PutDraftFiles: PUT /stacks/{name}/versions/{n}/files
func (c *Client) PutDraftFiles(ctx context.Context, name string, versionNumber int64, files []StackFile) (*PutFilesResponse, error) {
	body, err := json.Marshal(putFilesReq{Files: files})
	if err != nil {
		return nil, err
	}
	suffix := stackPath(name, "/versions/"+strconv.FormatInt(versionNumber, 10)+"/files")
	endpoint := c.scopedURL(suffix)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
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
	var out PutFilesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode put files: %w", err)
	}
	return &out, nil
}

// PatchDraftFile: PATCH /stacks/{name}/versions/{n}/files
//
// Single-file upsert with optimistic concurrency. baseHash is the
// content_hash the caller previously observed; pass "" to create a
// new file. The server returns 404 if you pass a non-empty baseHash
// for a path that doesn't exist, and 409 for hash mismatch or
// create-collision. decodeError surfaces those with their
// `current_hash` so the caller can present a useful conflict UI.
func (c *Client) PatchDraftFile(ctx context.Context, name string, versionNumber int64, path, content, baseHash string) (*PatchFileResponse, error) {
	body, err := json.Marshal(patchFileReq{Path: path, Content: content, BaseHash: baseHash})
	if err != nil {
		return nil, err
	}
	suffix := stackPath(name, "/versions/"+strconv.FormatInt(versionNumber, 10)+"/files")
	endpoint := c.scopedURL(suffix)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
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
	var out PatchFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode patch file: %w", err)
	}
	return &out, nil
}

// DeleteDraftFile: DELETE /stacks/{name}/versions/{n}/files
//
// baseHash is required (server returns 400 for blind deletes).
func (c *Client) DeleteDraftFile(ctx context.Context, name string, versionNumber int64, path, baseHash string) (*DeleteFileResponse, error) {
	body, err := json.Marshal(deleteFileReq{Path: path, BaseHash: baseHash})
	if err != nil {
		return nil, err
	}
	suffix := stackPath(name, "/versions/"+strconv.FormatInt(versionNumber, 10)+"/files")
	endpoint := c.scopedURL(suffix)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, bytes.NewReader(body))
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
	var out DeleteFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode delete file: %w", err)
	}
	return &out, nil
}

// ValidateVersion: POST /stacks/{name}/versions/{n}/validate
//
// Server returns 200 with `{"ok": true|false, "errors": [...]}` so a
// failed parse is not an HTTP error — the caller decides whether
// `!ok` should block a downstream activate.
func (c *Client) ValidateVersion(ctx context.Context, name string, versionNumber int64) (*ValidateResponse, error) {
	suffix := stackPath(name, "/versions/"+strconv.FormatInt(versionNumber, 10)+"/validate")
	endpoint := c.scopedURL(suffix)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
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
	var out ValidateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode validate: %w", err)
	}
	return &out, nil
}

// DiffVersions: GET /stacks/{name}/diff?v1=&v2=
func (c *Client) DiffVersions(ctx context.Context, name string, v1, v2 int64) (*DiffResponse, error) {
	endpoint := c.scopedURL(stackPath(name, "/diff")) +
		"?v1=" + strconv.FormatInt(v1, 10) +
		"&v2=" + strconv.FormatInt(v2, 10)
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
	var out DiffResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode diff: %w", err)
	}
	return &out, nil
}

// Activate: POST /stacks/{name}/activate
func (c *Client) Activate(ctx context.Context, name string, versionNumber int64) (*ActivateResponse, error) {
	body, err := json.Marshal(activateReq{VersionNumber: versionNumber})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL(stackPath(name, "/activate"))
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
	var out ActivateResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode activate: %w", err)
	}
	return &out, nil
}
