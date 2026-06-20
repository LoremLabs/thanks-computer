package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	reqBody := map[string]any{"args": args}
	if cursor != "" {
		reqBody["cursor"] = cursor
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/v1/cli"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
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
