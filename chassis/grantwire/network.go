package grantwire

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Header is where the run grant's token travels on the network: the one
// the capability inlet reads (chassis/processor RunGrantHeader).
const Header = "Txco-Run-Grant"

// SandboxPath is the route a program on another machine opens a sandbox
// at, on its parent's web head: POST /v1/sandbox/<name>, the token in
// Header, the body {"entry": …}. The answer is a Response.
const SandboxPath = "/v1/sandbox/"

// OpenURL asks the chassis at base (the parent's URL, as a node's
// --parent-url names it) to open one sandbox over HTTPS: the same request,
// the same answer and the same errors as Open over the socket. A nil
// client is a plain one with a 30-second ceiling.
func OpenURL(ctx context.Context, client *http.Client, base string, req Request) (Opened, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return Opened{}, fmt.Errorf("%w: %q is not the chassis's URL", ErrUnavailable, base)
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	body, _ := json.Marshal(struct {
		Entry string `json:"entry,omitempty"`
	}{req.Entry})
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String()+SandboxPath+url.PathEscape(req.Sandbox), bytes.NewReader(body))
	if err != nil {
		return Opened{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	hreq.Header.Set(Header, req.Token)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(hreq)
	if err != nil {
		return Opened{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
	if err != nil {
		return Opened{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(data) > MaxResponse {
		return Opened{}, fmt.Errorf("%w: the answer is too long", ErrUnavailable)
	}
	return decodeResponse(bytes.TrimSpace(data))
}
