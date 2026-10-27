package admin

import (
	"net/http"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/connection"
)

// Connection endpoints: the operator's read of a tenant's connections
// (chassis/connection) — who is connected where, with what status — and a
// revoke. Metadata only: nothing here reveals material, and there is no
// endpoint that could. Capabilities are the secrets', since a connection
// is a credential the chassis holds for the tenant.
//
//	GET    /v1/tenants/{t}/connections?principal=P | ?provider=N
//	DELETE /v1/tenants/{t}/connections?principal=P&provider=N

// SetConnections installs the connection service. nil ⇒ 503.
func (c *Controller) SetConnections(s *connection.Service) { c.connections = s }

type connectionRecord struct {
	ID               string   `json:"id"`
	Principal        string   `json:"principal"`
	Provider         string   `json:"provider"`
	Label            string   `json:"label,omitempty"`
	Kind             string   `json:"kind"`
	Subject          string   `json:"subject,omitempty"`
	Account          string   `json:"account,omitempty"`
	Status           string   `json:"status"`
	Scopes           []string `json:"scopes"`
	Generation       int64    `json:"generation"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
	RefreshExpiresAt string   `json:"refresh_expires_at,omitempty"`
	CreatedBy        string   `json:"created_by"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

type listConnectionsResponse struct {
	Connections []connectionRecord `json:"connections"`
}

func rowToRecord(r connection.Row) connectionRecord {
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	out := connectionRecord{
		ID: r.ID, Principal: r.PrincipalID, Provider: r.Provider, Label: r.Label, Kind: r.Kind,
		Subject: r.Subject, Account: r.Account, Status: r.Status, Scopes: r.Scopes, Generation: r.Generation,
		CreatedBy: r.CreatedBy, CreatedAt: stamp(r.CreatedAt), UpdatedAt: stamp(r.UpdatedAt),
	}
	if out.Scopes == nil {
		out.Scopes = []string{}
	}
	if r.ExpiresAt != nil {
		out.ExpiresAt = stamp(*r.ExpiresAt)
	}
	if r.RefreshExpiresAt != nil {
		out.RefreshExpiresAt = stamp(*r.RefreshExpiresAt)
	}
	return out
}

// handleListConnections: GET ?principal=P or ?provider=N (one of them).
func (c *Controller) handleListConnections(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	if c.connections == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "connections_unavailable", map[string]any{"reason": "no identity store or master key on this node"})
		return
	}
	principal, provider := r.URL.Query().Get("principal"), r.URL.Query().Get("provider")
	var (
		rows []connection.Row
		err  error
	)
	switch {
	case principal != "" && provider != "":
		writeJSONError(w, http.StatusBadRequest, "one_of_principal_or_provider", nil)
		return
	case principal != "":
		rows, err = c.connections.List(r.Context(), ac.TenantID, principal)
	case provider != "":
		rows, err = c.connections.ListProvider(r.Context(), ac.TenantID, provider)
	default:
		writeJSONError(w, http.StatusBadRequest, "principal_or_provider_required", nil)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", map[string]any{"error": err.Error()})
		return
	}
	out := listConnectionsResponse{Connections: make([]connectionRecord, 0, len(rows))}
	for _, row := range rows {
		out.Connections = append(out.Connections, rowToRecord(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevokeConnection: DELETE ?principal=P&provider=N. 204 when
// removed, 404 when there was none.
func (c *Controller) handleRevokeConnection(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "secret:*:write"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	if c.connections == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "connections_unavailable", map[string]any{"reason": "no identity store or master key on this node"})
		return
	}
	principal, provider := r.URL.Query().Get("principal"), r.URL.Query().Get("provider")
	if principal == "" || provider == "" {
		writeJSONError(w, http.StatusBadRequest, "principal_and_provider_required", nil)
		return
	}
	_, removed, cerr := c.connections.Revoke(r.Context(), ac.TenantSlug, ac.TenantID, principal, provider)
	if cerr != nil {
		status := http.StatusInternalServerError
		if cerr.Code == connection.CodeInvalidArg {
			status = http.StatusBadRequest
		}
		writeJSONError(w, status, "connection_"+cerr.Code, map[string]any{"error": cerr.Message})
		return
	}
	if !removed {
		writeJSONError(w, http.StatusNotFound, "connection_not_found", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
