package attached

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/coder/websocket"

	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
)

// The machine's side of the three routes. base is the chassis's address,
// scheme and host: https://attach.thanks.computer.

// APIError is a refusal from the chassis.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("attach: %s (%s, HTTP %d)", e.Message, e.Code, e.Status)
}

func apiError(resp *http.Response, body []byte) error {
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if out.Error.Code == "" {
		out.Error.Code, out.Error.Message = "http", strings.TrimSpace(string(body))
	}
	return &APIError{Status: resp.StatusCode, Code: out.Error.Code, Message: out.Error.Message}
}

func post(ctx context.Context, hc *http.Client, target string, body []byte, sign func(*http.Request) error, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if sign != nil {
		if err := sign(req); err != nil {
			return err
		}
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return apiError(resp, raw)
	}
	return json.Unmarshal(raw, out)
}

// PeekCode asks what a code is for, without spending it.
func PeekCode(ctx context.Context, hc *http.Client, base, code string) (CodeResult, error) {
	var out CodeResult
	body, _ := json.Marshal(map[string]string{"code": code})
	return out, post(ctx, hc, strings.TrimRight(base, "/")+PathCode, body, nil, &out)
}

// EnrollKey spends code and enrolls key for the code's workspace. The
// request is signed with key itself, over its exact body: possession of the
// private half is proved at the moment the public half is enrolled.
func EnrollKey(ctx context.Context, hc *http.Client, base string, key ed25519.PrivateKey, in EnrollBody) (EnrollResult, error) {
	var out EnrollResult
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return out, errors.New("attach: not an Ed25519 key")
	}
	in.PublicKey = pub
	body, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	sign := func(req *http.Request) error {
		return signature.NewSigner().Sign(req, signature.ActorPrivateKey{KeyID: Fingerprint(pub), PrivateKey: key})
	}
	return out, post(ctx, hc, strings.TrimRight(base, "/")+PathEnroll, body, sign, &out)
}

// Dial opens the connection a Link runs on, and returns it with the host it
// was dialed by: the name the machine signs into its challenge answer.
func Dial(ctx context.Context, hc *http.Client, base string) (net.Conn, string, error) {
	u, err := url.Parse(strings.TrimRight(base, "/") + PathLink)
	if err != nil {
		return nil, "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return nil, "", fmt.Errorf("attach: address %q needs http or https", base)
	}
	c, _, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: hc})
	if err != nil {
		return nil, "", err
	}
	c.SetReadLimit(maxMessage + 1<<20)
	// The connection's life is the caller's to end, by closing it.
	return websocket.NetConn(context.WithoutCancel(ctx), c, websocket.MessageBinary), u.Host, nil
}
