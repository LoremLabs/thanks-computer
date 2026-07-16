package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ErrCommandUnsupported is returned by RunCommand when the server does not
// implement the forwarded command (HTTP 404) — either it registers no commands
// (open core) or no handler matches args[0]. The CLI treats this as "fall back
// to the unknown-subcommand error" rather than surfacing it.
var ErrCommandUnsupported = errors.New("command not supported by this server")

// CommandResult mirrors the server's /v1/cli response: rendered output + a
// process-style exit code, plus the optional poll directive (see clicmd.Result).
type CommandResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
	// Cursor + PollAfterMs drive the forwarder's poll loop: when PollAfterMs > 0
	// the CLI re-runs the command after that delay, passing Cursor back.
	Cursor      string `json:"cursor,omitempty"`
	PollAfterMs int    `json:"poll_after_ms,omitempty"`
	// OpenURL asks the forwarder to open this URL in the browser (best-effort,
	// interactive terminals only) after printing Stdout.
	OpenURL string `json:"open_url,omitempty"`
	// AwaitCallback asks the forwarder to block on its loopback server after
	// opening OpenURL, until the hosted page redirects back or times out.
	AwaitCallback bool `json:"await_callback,omitempty"`
}

// RunCommand forwards a CLI argv (e.g. ["credit","grant","add","acme","500"]) to
// the server's /v1/cli dispatcher as a signed request, and returns the command's
// rendered output + exit code. A 404 maps to ErrCommandUnsupported so the caller
// can fall through gracefully; other non-200s return the decoded server error.
func (c *Client) RunCommand(ctx context.Context, args []string) (*CommandResult, error) {
	return c.RunCommandCursor(ctx, args, "")
}

// RunCommandCursor is RunCommand carrying a poll cursor for a pollable command.
// The cursor is sent ONLY when non-empty, so a first poll / non-pollable command
// produces the exact legacy request shape (older servers that DisallowUnknownFields
// never see an unexpected field).
func (c *Client) RunCommandCursor(ctx context.Context, args []string, cursor string) (*CommandResult, error) {
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/v1/cli"
	return c.runCommandAt(ctx, endpoint, args, cursor)
}

// RunTenantCommand forwards a CLI argv to the TENANT-scoped exec endpoint
// (/v1/tenants/{tenant}/cli) as a signed request — the door for self-serve
// overlay verbs (`credits`/`billing`) that must run as the tenant, not
// super-admin. Same 404→ErrCommandUnsupported contract as RunCommand.
func (c *Client) RunTenantCommand(ctx context.Context, tenant string, args []string) (*CommandResult, error) {
	return c.RunTenantCommandCursor(ctx, tenant, args, "")
}

// RunTenantCommandCursor is RunTenantCommand carrying a poll cursor.
func (c *Client) RunTenantCommandCursor(ctx context.Context, tenant string, args []string, cursor string) (*CommandResult, error) {
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/v1/tenants/" + url.PathEscape(tenant) + "/cli"
	return c.runCommandAt(ctx, endpoint, args, cursor)
}

// runCommandAt POSTs the argv (+ optional cursor) to a /…/cli endpoint and
// decodes the result. Shared by the admin (/v1/cli) and tenant
// (/v1/tenants/{t}/cli) forwarders — only the endpoint differs.
func (c *Client) runCommandAt(ctx context.Context, endpoint string, args []string, cursor string) (*CommandResult, error) {
	reqBody := map[string]any{"args": args}
	if cursor != "" {
		reqBody["cursor"] = cursor
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// Loopback callback (optional) rides as headers so it's backward-compatible:
	// an older server that doesn't read them simply won't AwaitCallback.
	if c.cbURL != "" {
		httpReq.Header.Set("X-Txco-Callback-URL", c.cbURL)
		httpReq.Header.Set("X-Txco-Callback-State", c.cbState)
	}
	if err := c.applyAuth(httpReq, body); err != nil {
		return nil, err
	}
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrCommandUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out CommandResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode command result: %w", err)
	}
	return &out, nil
}
