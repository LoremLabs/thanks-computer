// Package client is the small HTTP client the txco CLI uses to talk to a
// running chassis admin server.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
)

// Op mirrors the wire shape used by chassis/server/admin.OpRecord. Kept
// duplicated rather than imported so the CLI doesn't pull in the server
// package transitively.
type Op struct {
	Stack   string `json:"stack"`
	Scope   int    `json:"scope"`
	Name    string `json:"name"`
	Txcl    string `json:"txcl"`
	MockReq string `json:"mock_req,omitempty"`
	MockRes string `json:"mock_res,omitempty"`
}

type ListResponse struct {
	Ops []Op `json:"ops"`
}

type ErrorResponse struct {
	Error  string         `json:"error"`
	Detail map[string]any `json:"detail,omitempty"`
}

// Target is the destination chassis — URL plus optional basic auth.
// When Auth is non-nil, signed requests take precedence over basic.
//
// Auth holds a signer.Signer (interface), not a concrete struct, so
// the chosen backend (file key, ssh-agent, future hardware) plugs in
// at this seam without the client knowing which it talks to.
//
// Tenant is the slug under which tenant-scoped requests are issued.
// Empty means "talk to the legacy flat routes" — useful for
// chassis-wide endpoints (whoami, key revoke) where there's no
// tenant context, and for callers that haven't migrated yet. Phase 2
// keeps both URL shapes wired on the server.
type Target struct {
	Addr   string
	User   string
	Pass   string
	Tenant string
	Auth   signer.Signer
}

// Client wraps an *http.Client and a Target.
type Client struct {
	http   *http.Client
	target Target
}

func New(t Target) *Client {
	return &Client{
		http:   &http.Client{Timeout: 30 * time.Second},
		target: t,
	}
}

// do is the single chokepoint every Client call uses to send an HTTP
// request. Wraps http.Client.Do so network-layer failures (dial
// refused, DNS miss, timeout) get translated into human-readable
// errors instead of Go's stock `Get "http://...": dial tcp …: connect:
// connection refused` text. HTTP-status errors stay where they were
// (decodeError, called by the per-endpoint methods on a non-2xx
// response) — this only catches errors that come back from the
// transport layer.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	v := verboseEnabled()
	var start time.Time
	if v {
		start = time.Now()
		fmt.Fprintf(os.Stderr, "[txco] → %s %s%s\n", req.Method, req.URL.String(), authTag(req))
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if v {
			fmt.Fprintf(os.Stderr, "[txco] ✗ %v\n", err)
		}
		return nil, prettifyNetworkError(err, c.target.Addr)
	}
	if v {
		fmt.Fprintf(os.Stderr, "[txco] ← %s (%dms)\n", resp.Status, time.Since(start).Milliseconds())
		// Surface the error body (chassis JSON error, or a Cloudflare
		// 5xx HTML page) — the thing you actually need when debugging
		// a 502/"is this even prod". Buffer + restore so the normal
		// decodeError path is unaffected. Only on failures; success
		// bodies (large list/version responses) are left streaming.
		if resp.StatusCode >= 400 && resp.Body != nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(b))
			if s := strings.TrimSpace(string(b)); s != "" {
				if len(s) > 2048 {
					s = s[:2048] + "…(truncated)"
				}
				fmt.Fprintf(os.Stderr, "[txco]   body: %s\n", s)
			}
		}
	}
	return resp, nil
}

// verboseEnabled gates request tracing. Env-driven so EVERY txco
// command honours it with zero per-command wiring:
// `TXCO_VERBOSE=1 txco <anything>`. The --verbose flag on apply just
// sets this env.
func verboseEnabled() bool {
	switch os.Getenv("TXCO_VERBOSE") {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

// authTag reports the auth mode for the trace line WITHOUT exposing any
// secret values (never prints Authorization/Signature contents).
func authTag(req *http.Request) string {
	if req.Header.Get("Signature-Input") != "" {
		return " (signed)"
	}
	if strings.HasPrefix(req.Header.Get("Authorization"), "Basic ") {
		return " (basic)"
	}
	return ""
}

// ListOps returns rules at-or-under the given stack prefix. Empty prefix
// returns all rules.
func (c *Client) ListOps(ctx context.Context, prefix string) ([]Op, error) {
	endpoint := c.scopedURL("/ops")
	if prefix != "" {
		endpoint += "?stack=" + url.QueryEscape(prefix)
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
	var out ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list response: %w", err)
	}
	return out.Ops, nil
}

// (ImportOps removed — the legacy /ops/import endpoint is retired.
// CLI callers push via CreateDraft + PutDraftFiles + Activate.)

// applyAuth picks the strongest configured credential and applies it.
// Signed > basic > none. For signed requests the body bytes are
// required for the Content-Digest header; callers pass them in (nil
// is fine for GETs / no-body requests).
//
// The signed branch is one polymorphic call into whichever
// signer.Signer the target carries — FileKeySigner, AgentSigner, or
// any future backend that implements the interface.
func (c *Client) applyAuth(req *http.Request, body []byte) error {
	if c.target.Auth != nil {
		// Make sure net/http can still read the body after the
		// signer hashes it: hand it a fresh reader before the call.
		if len(body) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
		if err := c.target.Auth.Sign(req, body); err != nil {
			return fmt.Errorf("sign request: %w", err)
		}
		// And restore the body again — Sign() must NOT touch it
		// (our canonicalizer reads body bytes directly, not from
		// req.Body), but be defensive in case a future backend does.
		if len(body) > 0 {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
		return nil
	}
	if c.target.User != "" {
		req.SetBasicAuth(c.target.User, c.target.Pass)
	}
	return nil
}

// Addr returns the configured chassis URL. Useful for auth commands
// that want to echo the URL they're talking to without re-resolving.
func (c *Client) Addr() string { return c.target.Addr }

// scopedURL builds the URL for an endpoint that lives under
// /v1/tenants/{tenant}/… `suffix` is the slash-prefixed path under
// that prefix (e.g. "/ops", "/auth/invitations").
//
// When Target.Tenant is empty (e.g. tests that construct Target{}
// directly without going through ResolveTenant) the helper falls
// back to the literal "default" — same bottom rung as
// auth.ResolveTenant. This means a misconfigured caller hits the
// seeded default tenant rather than the retired flat routes.
func (c *Client) scopedURL(suffix string) string {
	tenant := c.target.Tenant
	if tenant == "" {
		tenant = "default"
	}
	return strings.TrimRight(c.target.Addr, "/") +
		"/v1/tenants/" + url.PathEscape(tenant) + suffix
}

// --- auth endpoints --------------------------------------------------------

// DevEnrollRequest is the wire body /auth/dev/enroll expects.
type DevEnrollRequest struct {
	PublicKeyB64 string `json:"public_key_b64"`
	Algorithm    string `json:"algorithm"`
	Label        string `json:"label,omitempty"`
	Kind         string `json:"kind,omitempty"`
}

// DevEnrollResponse is the wire response /auth/dev/enroll returns.
type DevEnrollResponse struct {
	ActorID      string   `json:"actor_id"`
	KeyID        string   `json:"key_id"`
	Capabilities []string `json:"capabilities"`
	// TenantSlug is the tenant the new actor was placed in. Always
	// "default" for bootstrap-local; the invitation's tenant for
	// accept. Empty if the server is older than phase 5; the CLI
	// falls back to "default" in that case.
	TenantSlug string `json:"tenant_slug,omitempty"`
	// SuperAdmin reports whether this enrolment yielded chassis-wide
	// super-admin (first-boot bootstrap only).
	SuperAdmin bool `json:"super_admin,omitempty"`
}

// DevEnroll exchanges a shared secret for an actor + key in one shot.
// Unsigned (the endpoint is unprotected); we send X-Txco-Enroll-Secret
// as the bootstrap credential.
func (c *Client) DevEnroll(ctx context.Context, secret string, body DevEnrollRequest) (*DevEnrollResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/auth/dev/enroll"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Txco-Enroll-Secret", secret)
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out DevEnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode enroll response: %w", err)
	}
	return &out, nil
}

// WhoamiResponse mirrors chassis/server/admin.whoamiResponse.
type WhoamiResponse struct {
	Source       string             `json:"source"`
	ActorID      string             `json:"actor_id,omitempty"`
	KeyID        string             `json:"key_id,omitempty"`
	Label        string             `json:"label,omitempty"`
	SuperAdmin   bool               `json:"super_admin,omitempty"`
	Capabilities []string           `json:"capabilities"`
	Memberships  []WhoamiMembership `json:"memberships,omitempty"`
}

// WhoamiMembership is one row of the per-tenant grants for the
// caller, returned alongside identity.
type WhoamiMembership struct {
	TenantID     string   `json:"tenant_id"`
	TenantSlug   string   `json:"tenant_slug"`
	Capabilities []string `json:"capabilities"`
}

// Whoami calls GET /auth/whoami with whatever credentials the target
// has configured. Useful to confirm signed-auth wiring.
func (c *Client) Whoami(ctx context.Context) (*WhoamiResponse, error) {
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/auth/whoami"
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
	var out WhoamiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode whoami response: %w", err)
	}
	return &out, nil
}

// RevokeResponse mirrors chassis/server/admin.revokeResponse.
type RevokeResponse struct {
	Revoked bool   `json:"revoked"`
	ActorID string `json:"actor_id,omitempty"`
	KeyID   string `json:"key_id,omitempty"`
}

// RevokeKey signs a POST /auth/keys/<id>/revoke.
func (c *Client) RevokeKey(ctx context.Context, keyID string) (*RevokeResponse, error) {
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/auth/keys/" + url.PathEscape(keyID) + "/revoke"
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
	var out RevokeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode revoke response: %w", err)
	}
	return &out, nil
}

// --- invitations -----------------------------------------------------------

// CreateInvitationRequest is the wire body POST /auth/invitations expects.
//
// Capabilities is the list of 3-segment capability strings the
// resulting member receives. Empty defaults to admin:all on the
// server (back-compat with pre-phase-6 CLIs).
type CreateInvitationRequest struct {
	Label        string   `json:"label,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	TTLSeconds   int      `json:"ttl_seconds,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// CreateInvitationResponse is what the server returns once the token
// is minted. Token is the one-shot raw secret — it never appears in
// the DB or in any subsequent response.
type CreateInvitationResponse struct {
	InvitationID string `json:"invitation_id"`
	Token        string `json:"token"`
	ExpiresAt    string `json:"expires_at"`
}

// Invitation mirrors the server's invitation row for listing.
type Invitation struct {
	InvitationID string   `json:"invitation_id"`
	Label        string   `json:"label,omitempty"`
	Kind         string   `json:"kind,omitempty"`
	Capabilities []string `json:"capabilities"`
	CreatedBy    string   `json:"created_by"`
	CreatedAt    string   `json:"created_at"`
	ExpiresAt    string   `json:"expires_at"`
	ConsumedAt   *string  `json:"consumed_at,omitempty"`
	ConsumedBy   string   `json:"consumed_by,omitempty"`
	RevokedAt    *string  `json:"revoked_at,omitempty"`
	Status       string   `json:"status"`
}

type listInvitationsResponse struct {
	Invitations []Invitation `json:"invitations"`
}

// ConsumeInvitationRequest is the wire body POST
// /auth/invitations/consume expects. Unsigned endpoint — the token IS
// the authentication.
type ConsumeInvitationRequest struct {
	Token        string `json:"token"`
	PublicKeyB64 string `json:"public_key_b64"`
	Algorithm    string `json:"algorithm"`
	Label        string `json:"label,omitempty"`
	Kind         string `json:"kind,omitempty"`
}

// RevokeInvitationResponse mirrors the server's revoke response shape.
type RevokeInvitationResponse struct {
	Revoked      bool   `json:"revoked"`
	InvitationID string `json:"invitation_id"`
}

// CreateInvitation mints a new invitation. Signed.
func (c *Client) CreateInvitation(ctx context.Context, req CreateInvitationRequest) (*CreateInvitationResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/auth/invitations")
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out CreateInvitationResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode create-invitation: %w", err)
	}
	return &out, nil
}

// ListInvitations returns all invitations (newest first). Signed.
func (c *Client) ListInvitations(ctx context.Context) ([]Invitation, error) {
	endpoint := c.scopedURL("/auth/invitations")
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
	var out listInvitationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list-invitations: %w", err)
	}
	return out.Invitations, nil
}

// RevokeInvitation marks an invitation revoked by id. Signed.
func (c *Client) RevokeInvitation(ctx context.Context, invitationID string) (*RevokeInvitationResponse, error) {
	suffix := "/auth/invitations/" + url.PathEscape(invitationID) + "/revoke"
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
	var out RevokeInvitationResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode revoke-invitation: %w", err)
	}
	return &out, nil
}

// ConsumeInvitation redeems a token in exchange for a fresh actor +
// key. Unsigned endpoint; the token is the credential.
func (c *Client) ConsumeInvitation(ctx context.Context, req ConsumeInvitationRequest) (*DevEnrollResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/auth/invitations/consume"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out DevEnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode consume-invitation: %w", err)
	}
	return &out, nil
}

// OAuthEnrollRequest is the body for POST /auth/oauth/enroll. IDToken is a
// bearer secret — do not log this struct.
type OAuthEnrollRequest struct {
	IDToken    string `json:"id_token"`
	PublicKey  string `json:"public_key"`
	Label      string `json:"label,omitempty"`
	Profile    string `json:"profile,omitempty"`
	TenantSlug string `json:"tenant_slug,omitempty"`
}

// OAuthEnrollResponse mirrors the enroll endpoint's success body. ChassisURL
// is the BASE admin URL to write into the CLI profile.
type OAuthEnrollResponse struct {
	ChassisURL   string   `json:"chassis_url"`
	TenantSlug   string   `json:"tenant_slug"`
	ActorID      string   `json:"actor_id"`
	KeyID        string   `json:"key_id"`
	Capabilities []string `json:"capabilities"`
}

// OAuthEnroll POSTs to the full enroll endpoint URL (resolved by the caller
// from cloud discovery / flags — the CLI never guesses the path), exchanging a
// verified id_token + ed25519 public key for a tenant + actor/key. Unsigned;
// the id_token is the credential. On a non-200 it returns *HTTPError so callers
// can branch on StatusCode/Code — e.g. a 409 `tenant_slug_required` carrying
// Detail["suggested_tenant_slug"].
func (c *Client) OAuthEnroll(ctx context.Context, endpointURL string, req OAuthEnrollRequest) (*OAuthEnrollResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out OAuthEnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode oauth-enroll: %w", err)
	}
	return &out, nil
}

// --- tenants ---------------------------------------------------------------

// Tenant mirrors the server's tenant row for listing.
type Tenant struct {
	TenantID  string `json:"tenant_id"`
	Slug      string `json:"slug"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"created_at"`
}

type listTenantsResponse struct {
	Tenants []Tenant `json:"tenants"`
}

// CreateTenantRequest is the wire body POST /v1/tenants expects.
// Slug is the durable handle and is required; Name is optional
// display text.
type CreateTenantRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name,omitempty"`
}

// TenantMember is one row of GET /v1/tenants/{t}/auth/members.
type TenantMember struct {
	ActorID      string   `json:"actor_id"`
	Label        string   `json:"label,omitempty"`
	Capabilities []string `json:"capabilities"`
	CreatedAt    string   `json:"created_at"`
}

type listTenantMembersResponse struct {
	Members []TenantMember `json:"members"`
}

// ListTenants returns the tenants the caller can see. The chassis
// filters by membership (or returns all for super_admin); the client
// just renders. Signed.
func (c *Client) ListTenants(ctx context.Context) ([]Tenant, error) {
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/v1/tenants"
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
	var out listTenantsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list tenants: %w", err)
	}
	return out.Tenants, nil
}

// CreateTenant mints a new tenant. Server-side gated by super_admin
// (or basic-auth / open operator). Returns the new row including the
// generated tenant_id. Signed.
func (c *Client) CreateTenant(ctx context.Context, req CreateTenantRequest) (*Tenant, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/v1/tenants"
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out Tenant
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode create tenant: %w", err)
	}
	return &out, nil
}

// SuspendTenantRequest optionally overrides the status/reason a suspended
// tenant's requests return. Empty fields default to 402 / "payment_required".
type SuspendTenantRequest struct {
	DenyStatus int    `json:"deny_status,omitempty"`
	DenyReason string `json:"deny_reason,omitempty"`
}

// TenantRuntimeState is the operator-visible admission state returned by
// suspend/resume.
type TenantRuntimeState struct {
	TenantID   string `json:"tenant_id"`
	Slug       string `json:"slug"`
	Enabled    bool   `json:"enabled"`
	Suspended  bool   `json:"suspended"`
	DenyStatus int    `json:"deny_status"`
	DenyReason string `json:"deny_reason,omitempty"`
}

// SuspendTenant marks a tenant suspended so its requests are denied
// (deny_status, default 402) before its stack runs. super_admin. Signed.
func (c *Client) SuspendTenant(ctx context.Context, slug string, req SuspendTenantRequest) (*TenantRuntimeState, error) {
	return c.postTenantRuntimeState(ctx, slug, "suspend", req)
}

// ResumeTenant clears a tenant's suspension (back to admit). super_admin. Signed.
func (c *Client) ResumeTenant(ctx context.Context, slug string) (*TenantRuntimeState, error) {
	return c.postTenantRuntimeState(ctx, slug, "resume", SuspendTenantRequest{})
}

func (c *Client) postTenantRuntimeState(ctx context.Context, slug, action string, req SuspendTenantRequest) (*TenantRuntimeState, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/v1/tenants/" + url.PathEscape(slug) + "/" + action
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out TenantRuntimeState
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode tenant runtime state: %w", err)
	}
	return &out, nil
}

// FleetResyncRequest targets one tenant by slug for a control-plane re-emit.
type FleetResyncRequest struct {
	TenantSlug string `json:"tenant_slug"`
}

// FleetResyncCounts reports how many of each event type were queued.
type FleetResyncCounts struct {
	TenantCreated  int `json:"tenant_created"`
	HostnameBound  int `json:"hostname_bound"`
	StackActivated int `json:"stack_activated"`
}

// FleetResyncResponse is the summary POST /v1/fleet/resync returns.
type FleetResyncResponse struct {
	FleetEnabled bool              `json:"fleet_enabled"`
	TenantSlug   string            `json:"tenant_slug,omitempty"`
	Events       FleetResyncCounts `json:"events"`
}

// FleetResync re-emits a tenant's current control-plane state (its row +
// hostnames + active stack versions) as fresh fleet-sync events so lagging
// replicas converge. Non-destructive (upserts only). Server-side gated by
// super_admin. Signed.
func (c *Client) FleetResync(ctx context.Context, req FleetResyncRequest) (*FleetResyncResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/v1/fleet/resync"
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out FleetResyncResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode fleet-resync: %w", err)
	}
	return &out, nil
}

// GrantMemberRequest is the wire body for POST .../auth/members. The
// server treats this as an upsert: re-granting an existing member
// replaces the capability set (and clears revoked_at).
type GrantMemberRequest struct {
	ActorID      string   `json:"actor_id"`
	Capabilities []string `json:"capabilities"`
}

// GrantMember upserts a membership in the caller's target tenant.
// Server gates on actor:*:invite in the tenant. Returns the freshly-
// written row so callers can echo "granted actor_X: opstack:*:read"
// without a follow-up read.
func (c *Client) GrantMember(ctx context.Context, req GrantMemberRequest) (*TenantMember, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/auth/members")
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out TenantMember
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode grant member: %w", err)
	}
	return &out, nil
}

// RevokeMember soft-deletes a membership for the given actor in the
// caller's target tenant. Server gates on actor:*:invite. Idempotent
// — revoking an absent/already-revoked membership returns ok.
func (c *Client) RevokeMember(ctx context.Context, actorID string) error {
	suffix := "/auth/members/" + url.PathEscape(actorID)
	endpoint := c.scopedURL(suffix)
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

// ListTenantMembers returns the active membership rows for the
// caller's target tenant (set via Target.Tenant). Server enforces
// actor:read in this tenant. Signed.
func (c *Client) ListTenantMembers(ctx context.Context) ([]TenantMember, error) {
	endpoint := c.scopedURL("/auth/members")
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
	var out listTenantMembersResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list members: %w", err)
	}
	return out.Members, nil
}

// --- hostnames -------------------------------------------------------------

// Hostname mirrors a tenant_hostnames row returned by the admin API.
type Hostname struct {
	ID         string `json:"id"`
	Hostname   string `json:"hostname"`
	TenantID   string `json:"tenant_id"`
	Stack      string `json:"stack"`
	CreatedAt  string `json:"created_at"`
	CreatedBy  string `json:"created_by,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	VerifiedAt string `json:"verified_at,omitempty"`
}

// HostnameChallenge mirrors a fresh challenge row + its operator-
// facing instructions string.
type HostnameChallenge struct {
	ID           string `json:"id"`
	Method       string `json:"method"`
	Token        string `json:"token"`
	ExpiresAt    string `json:"expires_at"`
	Instructions string `json:"instructions"`
	// Reused is true when the server returned a pre-existing active
	// challenge (idempotent path, status 200) instead of minting a new
	// one. The CLI prints a "reusing active challenge" note in that
	// case instead of implying rotation.
	Reused bool `json:"reused,omitempty"`
	// Rotated is true when the server revoked a prior active token to
	// mint this one (only when the request set force=true, status 201).
	// The CLI prints a loud "previous token revoked — update your DNS"
	// warning so the operator notices their existing TXT is now stale.
	Rotated bool `json:"rotated,omitempty"`
}

// HostnameStatus is the read-only state-of-the-hostname view returned
// by GET /hostnames/{hostname}/status. Lets the CLI show the current
// verify token (and verified state) WITHOUT mutating anything —
// pre-status, the only way to "see" the token was to call challenge,
// which rotated it (internal docs/todo-custom-domains.md §6a).
type HostnameStatus struct {
	Hostname
	ActiveChallenges []HostnameStatusChallenge `json:"active_challenges,omitempty"`
}

// HostnameStatusChallenge is one entry of HostnameStatus.ActiveChallenges
// — same shape as HostnameChallenge minus the issuance-only Reused /
// Rotated flags, plus diagnostic fields (expired, last error from the
// previous verify attempt).
type HostnameStatusChallenge struct {
	ID           string `json:"id"`
	Method       string `json:"method"`
	Token        string `json:"token"`
	ExpiresAt    string `json:"expires_at"`
	Expired      bool   `json:"expired,omitempty"`
	AttemptedAt  string `json:"attempted_at,omitempty"`
	LastError    string `json:"last_error,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// HostnameVerifyResponse mirrors the /verify endpoint's success body.
type HostnameVerifyResponse struct {
	VerifiedAt string `json:"verified_at"`
	Method     string `json:"method"`
}

type listHostnamesResponse struct {
	Hostnames []Hostname `json:"hostnames"`
}

// AddHostnameRequest is the wire body POST /v1/tenants/{t}/hostnames
// expects. Hostname is canonicalized server-side; the response shows
// the canonical form back.
type AddHostnameRequest struct {
	Hostname string `json:"hostname"`
	Stack    string `json:"stack"`
}

// ListHostnames returns the active hostnames bound to the target
// tenant. `history=true` flips to all-rows mode (including revoked
// ones) for debugging "who used to own this?".
func (c *Client) ListHostnames(ctx context.Context, history bool) ([]Hostname, error) {
	endpoint := c.scopedURL("/hostnames")
	if history {
		endpoint += "?history=true"
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
	var out listHostnamesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list hostnames: %w", err)
	}
	return out.Hostnames, nil
}

// AddHostname claims a hostname for the target tenant against a
// specific stack. Server canonicalizes the hostname (lowercase, port-
// stripped, trailing-dot-stripped) so the returned row may differ
// from the request literal.
func (c *Client) AddHostname(ctx context.Context, req AddHostnameRequest) (*Hostname, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/hostnames")
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
	if resp.StatusCode != http.StatusCreated {
		return nil, decodeError(resp)
	}
	var h Hostname
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("decode add hostname: %w", err)
	}
	return &h, nil
}

// AttachHostname binds an existing hostname (claimed earlier via
// AddHostname without --stack, or being re-pointed at a different
// stack) to the given stack within the active tenant.
func (c *Client) AttachHostname(ctx context.Context, hostname, stack string) (*Hostname, error) {
	body, err := json.Marshal(map[string]string{"stack": stack})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/hostnames/" + url.PathEscape(hostname) + "/attach")
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var h Hostname
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("decode attach: %w", err)
	}
	return &h, nil
}

// CreateHostnameChallenge issues a fresh DNS-TXT or HTTP-01 challenge
// for the named hostname. Server returns the token + human-readable
// setup instructions the CLI prints verbatim.
func (c *Client) CreateHostnameChallenge(ctx context.Context, hostname, method string, force bool) (*HostnameChallenge, error) {
	body, err := json.Marshal(map[string]any{"method": method, "force": force})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/hostnames/" + url.PathEscape(hostname) + "/challenges")
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
	// 200 OK = idempotent reuse, 201 Created = fresh mint (incl. force
	// rotation). Decode either; the server stamps Reused/Rotated.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, decodeError(resp)
	}
	var out HostnameChallenge
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode challenge: %w", err)
	}
	return &out, nil
}

// HostnameStatusOf reads the read-only state of a hostname (binding +
// current active/expired challenges) without rotating anything.
// Counterpart to CreateHostnameChallenge for "I just want to see what
// token is in DNS today." See internal docs/todo-custom-domains.md §6a.
func (c *Client) HostnameStatusOf(ctx context.Context, hostname string) (*HostnameStatus, error) {
	endpoint := c.scopedURL("/hostnames/" + url.PathEscape(hostname) + "/status")
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
	var out HostnameStatus
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode hostname status: %w", err)
	}
	return &out, nil
}

// VerifyHostname runs the active challenge for the named hostname
// (either DNS or HTTP method, whichever was issued). On success
// returns the verified_at timestamp; on failure decodes the server's
// "verification_failed" body so the CLI can show last_error.
func (c *Client) VerifyHostname(ctx context.Context, hostname string) (*HostnameVerifyResponse, error) {
	endpoint := c.scopedURL("/hostnames/" + url.PathEscape(hostname) + "/verify")
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if err := c.applyAuth(httpReq, nil); err != nil {
		return nil, err
	}
	resp, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out HostnameVerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode verify: %w", err)
	}
	return &out, nil
}

// RemoveHostname soft-deletes a hostname binding. Idempotent — the
// server returns 200 with revoked=true whether or not the row was
// present.
func (c *Client) RemoveHostname(ctx context.Context, hostname string) error {
	endpoint := c.scopedURL("/hostnames/" + url.PathEscape(hostname))
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

// --- traces ----------------------------------------------------------------

// TraceListResponse mirrors chassis/server/admin.traceListResponse —
// a paginated list of recent traces returned by GET /traces/requests.json.
type TraceListResponse struct {
	Traces []TraceSummary `json:"traces"`
	Total  int            `json:"total"`
}

// TraceSummary is the per-trace summary used in the list view.
type TraceSummary struct {
	RID    string `json:"rid"`
	Src    string `json:"src,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	// Stack is the chassis trampoline stack the request enters at
	// (typically "boot/%/0"). Route is the destination of the first
	// stage.jump, if any — much more useful than Stack since every
	// request enters at the same trampoline.
	Stack      string `json:"stack,omitempty"`
	Route      string `json:"route,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	DurationMs *int64 `json:"duration_ms,omitempty"`
	Status     string `json:"status"`
}

// ListTraces fetches recent traces. See ListTracesETag for the
// ETag-aware variant; this thin wrapper is kept for callers that
// don't care about caching.
func (c *Client) ListTraces(ctx context.Context, limit int, grep string) (*TraceListResponse, error) {
	resp, _, _, err := c.ListTracesETag(ctx, limit, grep, "")
	return resp, err
}

// ListTracesETag fetches recent traces, sending If-None-Match when
// ifNoneMatch is non-empty. Returns (resp, etag, notModified, err):
//   - notModified=true means the server returned 304 and resp is nil;
//     the caller should keep using its cached copy.
//   - on a fresh 200 response, etag is the server's new ETag — pass
//     it on the next call to short-circuit if nothing changed.
//
// limit<=0 means the server default (50; server caps at 500). When
// grep is non-empty the server filters to traces whose files contain
// the substring (case-insensitive).
func (c *Client) ListTracesETag(ctx context.Context, limit int, grep, ifNoneMatch string) (*TraceListResponse, string, bool, error) {
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/traces/requests.json"
	params := url.Values{}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if grep != "" {
		params.Set("grep", grep)
	}
	if encoded := params.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, "", false, err
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	if err := c.applyAuth(req, nil); err != nil {
		return nil, "", false, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, resp.Header.Get("ETag"), true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		// Three reasons for a 404 here:
		//   - chassis is running an older binary that doesn't have
		//     this endpoint yet (common right after upgrading);
		//   - chassis is running with --trace-mode=off, which gates
		//     the whole /traces/ subtree;
		//   - the address points somewhere that isn't a chassis.
		// The CLI can't distinguish from a 404 alone, so surface all
		// three so the user knows where to look.
		return nil, "", false, fmt.Errorf("trace list endpoint not found (404) — restart the chassis if you just upgraded, or check that --trace-mode is set")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, decodeError(resp)
	}
	var out TraceListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", false, fmt.Errorf("decode trace list response: %w", err)
	}
	return &out, resp.Header.Get("ETag"), false, nil
}

// TraceResponse mirrors chassis/server/admin.traceRequestResponse — the
// aggregated view of one request returned by GET /traces/requests/{rid}.json.
type TraceResponse struct {
	RID    string `json:"rid"`
	Src    string `json:"src,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	// Stack is the boot trampoline; Route is the first stage.jump's
	// destination. See TraceSummary for the rationale.
	Stack            string         `json:"stack,omitempty"`
	Route            string         `json:"route,omitempty"`
	StartedAt        string         `json:"started_at,omitempty"`
	FinishedAt       string         `json:"finished_at,omitempty"`
	DurationMs       *int64         `json:"duration_ms,omitempty"`
	Status           string         `json:"status"`
	PayloadBytes     int64          `json:"payload_bytes,omitempty"`
	PayloadTruncated bool           `json:"payload_truncated,omitempty"`
	TraceMode        string         `json:"trace_mode,omitempty"`
	Steps            []TraceStep    `json:"steps"`
	In               map[string]any `json:"in,omitempty"`
	Out              any            `json:"out,omitempty"`
}

type TraceStep struct {
	Name            string `json:"name"`
	Operation       string `json:"operation,omitempty"`
	Transport       string `json:"transport,omitempty"`
	Stack           string `json:"stack,omitempty"`
	Scope           int    `json:"scope"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	DurationMs      int64  `json:"duration_ms"`
	Status          string `json:"status"`
	InputBytes      int64  `json:"input_bytes"`
	OutputBytes     int64  `json:"output_bytes"`
	InputTruncated  bool   `json:"input_truncated,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	Error           string `json:"error,omitempty"`
	In              any    `json:"in,omitempty"`
	Out             any    `json:"out,omitempty"`
}

// TraceNotFoundError marks a 404 from the trace endpoint so the caller
// can produce a friendly "no trace for rid …" message instead of the
// raw HTTP error.
type TraceNotFoundError struct{ RID string }

func (e *TraceNotFoundError) Error() string {
	return fmt.Sprintf("no trace for rid %q", e.RID)
}

// GetTrace fetches the aggregate trace document for rid. When full is
// true the response also embeds in/out payloads (only present when the
// chassis is running with --trace-mode=full).
//
// Returns *TraceNotFoundError if the server returns 404 — handlers can
// type-assert to print a friendlier error.
//
// Also returns the raw response bytes so callers that want to emit the
// untouched JSON (e.g. `txco trace --json`) don't have to re-encode.
func (c *Client) GetTrace(ctx context.Context, rid string, full bool) (*TraceResponse, []byte, error) {
	endpoint := strings.TrimRight(c.target.Addr, "/") + "/traces/requests/" + url.PathEscape(rid) + ".json"
	if full {
		endpoint += "?include=full"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	if err := c.applyAuth(req, nil); err != nil {
		return nil, nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, &TraceNotFoundError{RID: rid}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, decodeError(resp)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read trace response: %w", err)
	}
	var out TraceResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode trace response: %w", err)
	}
	return &out, body, nil
}

// HTTPError captures a server-side JSON error response in a typed
// form so callers can branch on status (`errors.As`) without parsing
// the rendered string. The `Code` field is the `error` value the
// chassis writes (e.g. `base_hash_mismatch`, `file_not_found`); the
// `Detail` map carries actionable context (current_hash, path, etc.).
type HTTPError struct {
	StatusCode int
	Status     string
	Code       string
	Detail     map[string]any
	Raw        string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("%s: %s (detail=%v)", e.Status, e.Code, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Status, e.Raw)
}

func decodeError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	out := &HTTPError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Raw:        strings.TrimSpace(string(body)),
	}
	var er ErrorResponse
	if json.Unmarshal(body, &er) == nil && er.Error != "" {
		out.Code = er.Error
		out.Detail = er.Detail
	}
	return out
}

// ---------- Per-tenant secret store ----------
//
// Wire shapes mirror chassis/server/admin/secret_endpoints.go. Two
// response shapes: `Secret` (metadata-only — for list, show, create,
// patch, rotate, revoke) and `SecretWithValue` (carries cleartext —
// ONLY for generate and rotate-generated; value is base64-url no-pad).
// See internal docs/todo-secret-store.md §5-§6.

// Secret is the metadata view of a tenant_secrets row. NEVER contains
// a value field — the only paths that return cleartext are
// GenerateSecret and RotateSecretGenerated, both via SecretWithValue.
type Secret struct {
	SecretID      string `json:"secret_id"`
	TenantID      string `json:"tenant_id"`
	Stack         string `json:"stack,omitempty"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by,omitempty"`
	LastRotatedAt string `json:"last_rotated_at,omitempty"`
	KeyVersion    int    `json:"key_version"`
	VersionNo     int    `json:"version_no"`
}

// SecretWithValue is the response shape from GenerateSecret and
// RotateSecretGenerated — the only two paths that return cleartext.
// `Value` is base64-url no-padding encoded random bytes.
type SecretWithValue struct {
	Secret Secret `json:"secret"`
	Value  string `json:"value"`
}

// CreateSecretRequest mirrors the JSON shape POST /secrets accepts.
// Value is the operator-supplied cleartext; the server NEVER returns
// it back in any response.
type CreateSecretRequest struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
	Stack       string `json:"stack,omitempty"`
}

// GenerateSecretRequest is the body for POST /secrets/generate. The
// server mints ByteLen random bytes (default 32) and returns the
// base64-url encoded value exactly once.
type GenerateSecretRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Stack       string `json:"stack,omitempty"`
	ByteLen     int    `json:"byte_len,omitempty"`
}

type listSecretsResponse struct {
	Secrets []Secret `json:"secrets"`
}

type secretResponse struct {
	Secret Secret `json:"secret"`
}

// ListSecrets returns active secrets in the active tenant (both
// tenant-wide and stack-scoped). Metadata only — the wire never
// carries a value field on this path.
func (c *Client) ListSecrets(ctx context.Context) ([]Secret, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.scopedURL("/secrets"), nil)
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
	var out listSecretsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode list secrets: %w", err)
	}
	return out.Secrets, nil
}

// GetSecret returns metadata for one secret. `stack` selects the
// stack-scoped row; empty string selects the tenant-wide row.
func (c *Client) GetSecret(ctx context.Context, name, stack string) (*Secret, error) {
	endpoint := c.scopedURL("/secrets/" + url.PathEscape(name))
	if stack != "" {
		endpoint += "?stack=" + url.QueryEscape(stack)
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
	var out secretResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode get secret: %w", err)
	}
	return &out.Secret, nil
}

// CreateSecret stores an operator-supplied value. Server returns
// metadata only — the operator already has the cleartext.
func (c *Client) CreateSecret(ctx context.Context, req CreateSecretRequest) (*Secret, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.scopedURL("/secrets"), bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusCreated {
		return nil, decodeError(resp)
	}
	var out secretResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode create secret: %w", err)
	}
	return &out.Secret, nil
}

// GenerateSecret mints a fresh random value server-side and returns
// it once. The caller is responsible for surfacing the value to the
// operator (e.g. printing to terminal) — no other endpoint can
// retrieve it later.
func (c *Client) GenerateSecret(ctx context.Context, req GenerateSecretRequest) (*SecretWithValue, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.scopedURL("/secrets/generate"), bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusCreated {
		return nil, decodeError(resp)
	}
	var out SecretWithValue
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode generate secret: %w", err)
	}
	return &out, nil
}

// UpdateSecretDescription PATCHes the description only. The server
// rejects any body that includes a `name` field (immutable per
// design §1.7).
func (c *Client) UpdateSecretDescription(ctx context.Context, name, stack, newDescription string) (*Secret, error) {
	body, err := json.Marshal(map[string]string{"description": newDescription})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/secrets/" + url.PathEscape(name))
	if stack != "" {
		endpoint += "?stack=" + url.QueryEscape(stack)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out secretResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode patch secret: %w", err)
	}
	return &out.Secret, nil
}

// RotateSecret writes a new version under the same name with an
// operator-supplied value.
func (c *Client) RotateSecret(ctx context.Context, name, stack, newValue string) (*Secret, error) {
	body, err := json.Marshal(map[string]string{"value": newValue})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/secrets/" + url.PathEscape(name) + "/rotate")
	if stack != "" {
		endpoint += "?stack=" + url.QueryEscape(stack)
	}
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out secretResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode rotate secret: %w", err)
	}
	return &out.Secret, nil
}

// RotateSecretGenerated mints a fresh random value, writes a new
// version under the same name, and returns the value once.
func (c *Client) RotateSecretGenerated(ctx context.Context, name, stack string, byteLen int) (*SecretWithValue, error) {
	body, err := json.Marshal(map[string]int{"byte_len": byteLen})
	if err != nil {
		return nil, err
	}
	endpoint := c.scopedURL("/secrets/" + url.PathEscape(name) + "/rotate-generated")
	if stack != "" {
		endpoint += "?stack=" + url.QueryEscape(stack)
	}
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
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out SecretWithValue
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode rotate-generated: %w", err)
	}
	return &out, nil
}

// RevokeSecret soft-deletes the active row. Returns nil on 204.
func (c *Client) RevokeSecret(ctx context.Context, name, stack string) error {
	endpoint := c.scopedURL("/secrets/" + url.PathEscape(name))
	if stack != "" {
		endpoint += "?stack=" + url.QueryEscape(stack)
	}
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
	if resp.StatusCode != http.StatusNoContent {
		return decodeError(resp)
	}
	return nil
}
