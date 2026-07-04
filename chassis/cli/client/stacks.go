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
	ManifestHash  string `json:"manifest_hash,omitempty"`
	// CodeManifestHash is the active version's manifest over CODE files only
	// (rules + FILES, no VECTORS/+KV/ packs). `txco apply` is code-only, so its
	// no-op short-circuit compares the local code manifest to THIS. Empty from an
	// older server → the client falls back to a normal deploy. See handleGetStack.
	CodeManifestHash string `json:"code_manifest_hash,omitempty"`
	CreatedAt        string `json:"created_at"`
	// MintHostname: false = headless (activate mints no auto routing URL),
	// true = mints one. Pointer so an OLDER server that doesn't send the
	// field decodes to nil ("unknown") rather than a misleading false — a
	// plain bool would label every stack headless against such a server.
	MintHostname *bool `json:"mint_hostname,omitempty"`
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
	// Encoding is "base64" when Content is base64-encoded — used for non-UTF-8
	// binary assets (images, fonts) that JSON would otherwise mangle. Empty =
	// raw UTF-8 text. The server decodes base64 before hashing/storing.
	//
	// Encoding "cas" marks a fingerprint-only row: Content is omitted and the
	// bytes MUST already be in the chassis content-addressed store under
	// ContentHash (streamed there via PutBlob). Used for DATASETS/ artifacts,
	// which can run to gigabytes and never ride the JSON body. The server
	// verifies presence and refuses the row when the hash is absent.
	Encoding string `json:"encoding,omitempty"`
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
	Files  []StackFile `json:"files"`
	Manage string      `json:"manage,omitempty"` // "code" | "data" | "all" (default)
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

// FileContent is the diagnostic + resolved bytes from GET /stacks/{name}/cat.
type FileContent struct {
	Stack           string `json:"stack"`
	Path            string `json:"path"`
	ActiveVersionID int64  `json:"active_version_id"`
	Found           bool   `json:"found"`
	Source          string `json:"source"`
	Reason          string `json:"reason"`
	ContentHash     string `json:"content_hash"`
	InlineLen       int    `json:"inline_len"`
	Size            int    `json:"size"`
	ContentB64      string `json:"content_b64"`
}

// CatFile: GET /stacks/{name}/cat?path=... — resolve a FILES/ asset of the
// stack's active version the way read-file does (manifest → CAS), for debugging.
func (c *Client) CatFile(ctx context.Context, name, filePath string) (*FileContent, error) {
	endpoint := c.scopedURL(stackPath(name, "/cat")) + "?path=" + url.QueryEscape(filePath)
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
	var out FileContent
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode cat: %w", err)
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

// PutDraftFiles: PUT /stacks/{name}/versions/{n}/files — replaces the whole
// file set (manage="all"). Used by `txco dev` (full local mirror).
func (c *Client) PutDraftFiles(ctx context.Context, name string, versionNumber int64, files []StackFile) (*PutFilesResponse, error) {
	return c.PutDraftFilesScoped(ctx, name, versionNumber, files, "")
}

// PutDraftFilesScoped is PutDraftFiles with an explicit manage scope: "code"
// replaces rules + FILES and carries the store-seed packs forward; "data"
// replaces VECTORS/+KV/ packs and carries the code forward; "" / "all" replaces
// everything. This is how data stays opt-in — `txco apply` uses "code".
func (c *Client) PutDraftFilesScoped(ctx context.Context, name string, versionNumber int64, files []StackFile, manage string) (*PutFilesResponse, error) {
	body, err := json.Marshal(putFilesReq{Files: files, Manage: manage})
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

// DeactivateStack retires a stack by activating an EMPTY version: it
// creates an empty draft (no ops/files) and activates it, so the stack
// stops serving (HTTP 404 / mail 550 / no matching ops) while its version
// history stays intact and re-deployable.
//
// This is the fleet-safe "stop serving": it rides the normal stack
// activation path, so every node converges the same way `apply` does —
// no bespoke control event that an older node couldn't apply.
//
// Use it when you've removed a stack from your local OPS/ tree: `apply`
// only re-versions the stacks it still finds, so a deleted stack keeps
// serving its last active version until it's deactivated here.
func (c *Client) DeactivateStack(ctx context.Context, name string) (*ActivateResponse, error) {
	v, err := c.CreateDraft(ctx, name, "") // empty draft (no clone of active)
	if err != nil {
		return nil, fmt.Errorf("create empty draft: %w", err)
	}
	if _, err := c.PutDraftFiles(ctx, name, v, nil); err != nil { // zero files → empty version
		return nil, fmt.Errorf("clear files: %w", err)
	}
	return c.Activate(ctx, name, v)
}

type stackSettingsReq struct {
	MintHostname *bool `json:"mint_hostname,omitempty"`
	Force        bool  `json:"force,omitempty"`
}

type StackSettingsResponse struct {
	MintHostname bool     `json:"mint_hostname"`
	RevokedHosts []string `json:"revoked_hosts,omitempty"`
}

// SetStackHostMint flips the per-stack auto-URL gate (stacks.mint_hostname) via
// PATCH /stacks/{name}/settings. mint=false makes activate skip the auto-minted
// routing hostname (`txco stack set --no-host`); mint=true re-enables it. The
// stack row is vivified server-side if it doesn't exist yet, so this can be set
// before the stack's first apply.
//
// If the stack already has a live chassis-minted URL, mint=false requires
// force=true (the server returns 409 "live_url_exists" otherwise) and revokes
// that host; the revoked hostnames come back in RevokedHosts.
func (c *Client) SetStackHostMint(ctx context.Context, name string, mint, force bool) (*StackSettingsResponse, error) {
	body, err := json.Marshal(stackSettingsReq{MintHostname: &mint, Force: force})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL(stackPath(name, "/settings"))
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
	var out StackSettingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode stack settings: %w", err)
	}
	return &out, nil
}

type batchStackSettingsReq struct {
	Match        string `json:"match"`
	MintHostname *bool  `json:"mint_hostname,omitempty"`
	Force        bool   `json:"force,omitempty"`
}

// BatchStackSettingsResponse is the result of a bulk mint-gate flip.
type BatchStackSettingsResponse struct {
	Matched      int      `json:"matched"`       // number of stacks whose gate was set
	MintHostname bool     `json:"mint_hostname"` // the value applied
	RevokedHosts []string `json:"revoked_hosts,omitempty"`
}

// BatchSetStackHostMint flips the auto-URL gate on EVERY stack whose name
// contains `match`, via POST /stacks/settings — the bulk sibling of
// SetStackHostMint, done in one server tx + one reload. Like the per-stack call,
// mint=false on stacks that already have live URLs requires force=true (the
// server returns 409 "live_url_exists" — with a `count` + sample `stacks` —
// otherwise) and revokes those hosts.
func (c *Client) BatchSetStackHostMint(ctx context.Context, match string, mint, force bool) (*BatchStackSettingsResponse, error) {
	body, err := json.Marshal(batchStackSettingsReq{Match: match, MintHostname: &mint, Force: force})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/stacks/settings")
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
	var out BatchStackSettingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode batch stack settings: %w", err)
	}
	return &out, nil
}
