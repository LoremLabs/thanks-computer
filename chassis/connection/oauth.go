package connection

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// The wire calls to a provider. Each goes to one kind of destination with
// the credentials that destination may see (docs/todo-connections.md §5):
// the token destination sees the client credentials, a code and a refresh
// token, never an access token; an api destination sees an access token,
// never the client credentials.

// answerMax bounds what is read of a provider's answer.
const answerMax = 64 << 10

// clientCreds is the provider's client, materialized for one request.
type clientCreds struct {
	id, secret string
}

// tokenAnswer is a token destination's answer, JSON or form-encoded
// (GitHub's default is the form).
type tokenAnswer struct {
	Access           string
	Refresh          string
	TokenType        string
	Scope            string
	ExpiresIn        int64
	RefreshExpiresIn int64
	Err              string // the OAuth error code, when refused
	ErrDescription   string
	Status           int
}

// outcome is what became of a request, as renewal needs to know it.
type outcome int

const (
	outcomeOK        outcome = iota
	outcomeRefused           // a definite refusal (invalid_grant, a 4xx with an error)
	outcomeNotSent           // the request never left (dial refused, DNS)
	outcomeUncertain         // it may have arrived: a timeout, a reset, a 5xx
)

// classify sorts a transport error: a dial that never connected means the
// request was not sent; anything after that is uncertain.
func classify(err error) outcome {
	var ue *url.Error
	if errors.As(err, &ue) {
		var oe *net.OpError
		if errors.As(ue.Err, &oe) && oe.Op == "dial" {
			return outcomeNotSent
		}
		var de *net.DNSError
		if errors.As(ue.Err, &de) {
			return outcomeNotSent
		}
	}
	return outcomeUncertain
}

// tokenRequest posts form to the provider's token destination with the
// client's credentials, and reads the answer.
func (s *Service) tokenRequest(ctx context.Context, p *Provider, creds clientCreds, form url.Values) (tokenAnswer, outcome, error) {
	endpoint, err := url.Parse(p.Destinations.Token)
	if err != nil {
		return tokenAnswer{}, outcomeNotSent, err
	}
	o, err := OriginOf(endpoint)
	if err != nil {
		return tokenAnswer{}, outcomeNotSent, err
	}
	form.Set("client_id", creds.id)
	if p.ClientAuth == ClientAuthBody {
		form.Set("client_secret", creds.secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return tokenAnswer{}, outcomeNotSent, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", s.userAgent)
	if p.ClientAuth == ClientAuthBasic {
		req.SetBasicAuth(url.QueryEscape(creds.id), url.QueryEscape(creds.secret))
	}
	resp, err := s.clients.For(o).Do(req)
	if err != nil {
		return tokenAnswer{}, classify(err), fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, answerMax))
	if err != nil {
		return tokenAnswer{}, outcomeUncertain, fmt.Errorf("token answer: %w", err)
	}
	a := parseTokenAnswer(resp.Header.Get("Content-Type"), body)
	a.Status = resp.StatusCode
	switch {
	case resp.StatusCode >= 500:
		return a, outcomeUncertain, fmt.Errorf("token destination answered %d", resp.StatusCode)
	case a.Err != "":
		return a, outcomeRefused, fmt.Errorf("token destination refused: %s", a.Err)
	case resp.StatusCode >= 300 || resp.StatusCode < 200:
		// A redirect or a 4xx with no OAuth error: said no, in its own way.
		return a, outcomeRefused, fmt.Errorf("token destination answered %d", resp.StatusCode)
	case a.Access == "":
		return a, outcomeRefused, errors.New("token destination answered without an access token")
	}
	return a, outcomeOK, nil
}

// parseTokenAnswer reads JSON or a form. GitHub answers 200 with
// `error=bad_verification_code` when it refuses, so the error field decides
// before the status does.
func parseTokenAnswer(contentType string, body []byte) tokenAnswer {
	mt, _, _ := mime.ParseMediaType(contentType)
	var a tokenAnswer
	body = bytes.TrimSpace(body)
	// JSON by content type, or by shape: a refusal often comes back as
	// text/plain with a JSON body.
	if mt == "application/json" || (len(body) > 0 && body[0] == '{') {
		var j struct {
			Access           string          `json:"access_token"`
			Refresh          string          `json:"refresh_token"`
			TokenType        string          `json:"token_type"`
			Scope            string          `json:"scope"`
			ExpiresIn        json.RawMessage `json:"expires_in"`
			RefreshExpiresIn json.RawMessage `json:"refresh_token_expires_in"`
			Err              string          `json:"error"`
			ErrDescription   string          `json:"error_description"`
		}
		if json.Unmarshal(body, &j) == nil {
			a = tokenAnswer{Access: j.Access, Refresh: j.Refresh, TokenType: j.TokenType, Scope: j.Scope, Err: j.Err, ErrDescription: j.ErrDescription}
			a.ExpiresIn = jsonInt(j.ExpiresIn)
			a.RefreshExpiresIn = jsonInt(j.RefreshExpiresIn)
			return a
		}
	}
	v, err := url.ParseQuery(string(body))
	if err != nil {
		return a
	}
	a = tokenAnswer{
		Access: v.Get("access_token"), Refresh: v.Get("refresh_token"), TokenType: v.Get("token_type"), Scope: v.Get("scope"),
		Err: v.Get("error"), ErrDescription: v.Get("error_description"),
	}
	a.ExpiresIn, _ = strconv.ParseInt(v.Get("expires_in"), 10, 64)
	a.RefreshExpiresIn, _ = strconv.ParseInt(v.Get("refresh_token_expires_in"), 10, 64)
	return a
}

// jsonInt reads a number that some providers send as a string.
func jsonInt(raw json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// exchange swaps an authorization code for tokens.
func (s *Service) exchange(ctx context.Context, p *Provider, creds clientCreds, code, verifier, redirectURI string) (tokenAnswer, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}}
	if p.PKCE {
		form.Set("code_verifier", verifier)
	}
	a, _, err := s.tokenRequest(ctx, p, creds, form)
	return a, err
}

// refresh renews with a refresh token.
func (s *Service) refresh(ctx context.Context, p *Provider, creds clientCreds, refreshToken string) (tokenAnswer, outcome, error) {
	return s.tokenRequest(ctx, p, creds, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
}

// account asks the provider who signed in: a GET at the declared url with
// the new access token, reading the subject and name fields.
func (s *Service) account(ctx context.Context, p *Provider, access string) (subject, name string, err error) {
	if p.Account == nil {
		return "", "", nil
	}
	u, err := url.Parse(p.Account.URL)
	if err != nil {
		return "", "", err
	}
	o, err := OriginOf(u)
	if err != nil {
		return "", "", err
	}
	if !p.APIOrigin(o) {
		return "", "", fmt.Errorf("account url %s is not an api destination", o)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.clients.For(o).Do(req)
	if err != nil {
		return "", "", fmt.Errorf("account request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, answerMax))
	if err != nil {
		return "", "", fmt.Errorf("account answer: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", "", fmt.Errorf("account request answered %d", resp.StatusCode)
	}
	subject = gjson.GetBytes(body, p.Account.Subject).String()
	name = gjson.GetBytes(body, p.Account.Name).String()
	if subject == "" {
		return "", "", fmt.Errorf("account answer has no %q", p.Account.Subject)
	}
	return subject, name, nil
}

// revokeAt tells an RFC 7009 revocation destination the token is done.
// Best effort: the row is gone either way.
func (s *Service) revokeAt(ctx context.Context, p *Provider, creds clientCreds, token, hint string) error {
	if p.Destinations.Revocation == "none" || token == "" {
		return nil
	}
	endpoint, err := url.Parse(p.Destinations.Revocation)
	if err != nil {
		return err
	}
	o, err := OriginOf(endpoint)
	if err != nil {
		return err
	}
	form := url.Values{"token": {token}, "token_type_hint": {hint}, "client_id": {creds.id}}
	if p.ClientAuth == ClientAuthBody {
		form.Set("client_secret", creds.secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", s.userAgent)
	if p.ClientAuth == ClientAuthBasic {
		req.SetBasicAuth(url.QueryEscape(creds.id), url.QueryEscape(creds.secret))
	}
	resp, err := s.clients.For(o).Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("revocation answered %d", resp.StatusCode)
	}
	return nil
}
