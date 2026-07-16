package admin

import (
	"encoding/json"
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/clicmd"
)

type cliExecRequest struct {
	Args []string `json:"args"`
	// Cursor is the poll cursor for a POLLABLE command (empty on the first
	// call). Forwarded to the handler via clicmd.WithCursor.
	Cursor string `json:"cursor,omitempty"`
}

type cliExecResponse struct {
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Exit   int    `json:"exit"`
	// Cursor + PollAfterMs ask the forwarding CLI to re-poll (see clicmd.Result).
	Cursor      string `json:"cursor,omitempty"`
	PollAfterMs int    `json:"poll_after_ms,omitempty"`
	// OpenURL asks the forwarding CLI to open this URL in the browser.
	OpenURL string `json:"open_url,omitempty"`
	// AwaitCallback asks the forwarding CLI to block on its loopback server.
	AwaitCallback bool `json:"await_callback,omitempty"`
}

// handleCLIExec runs a server-side CLI command forwarded by the core CLI's
// zero-install plugin path (`txco <name> ...` → POST /v1/cli). The command is
// identified by args[0]; args[1:] are passed to the registered handler.
//
// Lookup happens BEFORE the super-admin gate on purpose: an unknown command
// always 404s (regardless of the caller's auth), so a plain typo or a command
// this server doesn't implement falls back cleanly to the CLI's
// unknown-subcommand error. A KNOWN command then requires super-admin (403
// otherwise). The only thing the pre-auth lookup reveals is whether a (non-
// secret) command name is registered; execution still requires super-admin.
func (c *Controller) handleCLIExec(w http.ResponseWriter, r *http.Request) {
	var req cliExecRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	if len(req.Args) == 0 {
		writeJSONError(w, http.StatusBadRequest, "args_required", nil)
		return
	}

	h, ok := clicmd.Lookup(req.Args[0])
	if !ok {
		// Not implemented here → 404. The CLI treats this as "fall through to
		// the unknown-subcommand error" (open core registers no commands).
		writeJSONError(w, http.StatusNotFound, "unknown_command", map[string]any{"command": req.Args[0]})
		return
	}

	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}

	ctx := clicmd.WithCursor(r.Context(), req.Cursor)
	if cbURL := r.Header.Get("X-Txco-Callback-URL"); cbURL != "" {
		ctx = clicmd.WithCallback(ctx, clicmd.CallbackInfo{URL: cbURL, State: r.Header.Get("X-Txco-Callback-State")})
	}
	res, err := h(ctx, req.Args[1:])
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "command_failed", map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cliExecResponse{
		Stdout:        res.Stdout,
		Stderr:        res.Stderr,
		Exit:          res.Exit,
		Cursor:        res.Cursor,
		PollAfterMs:   res.PollAfterMs,
		OpenURL:       res.OpenURL,
		AwaitCallback: res.AwaitCallback,
	})
}

// handleTenantCLIExec runs a TENANT-scoped server-side CLI command forwarded by
// the core CLI's zero-install path when the caller resolves a tenant
// (`txco credits buy` → POST /v1/tenants/{tenant}/cli). It mounts on the tenant
// subrouter, so resolveTenantMiddleware has already resolved the tenant and, for
// a signed non-super-admin, replaced Capabilities with THIS tenant's membership
// (empty for non-members). Unlike /v1/cli it does NOT require super-admin — it
// requires MEMBERSHIP (super-admin operator override, or a non-empty membership
// cap set) — so a tenant self-serves for its OWN tenant and cannot cross into
// another. Lookup happens BEFORE the membership gate (same rationale as
// handleCLIExec): an unknown command 404s regardless of auth, so the CLI falls
// back cleanly to the unknown-subcommand error. This is stricter than today's
// /credit/* HTTP endpoints; a per-op billing:*:purchase capability is the
// finer-grained follow-on.
func (c *Controller) handleTenantCLIExec(w http.ResponseWriter, r *http.Request) {
	var req cliExecRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	if len(req.Args) == 0 {
		writeJSONError(w, http.StatusBadRequest, "args_required", nil)
		return
	}

	h, ok := clicmd.LookupTenant(req.Args[0])
	if !ok {
		writeJSONError(w, http.StatusNotFound, "unknown_command", map[string]any{"command": req.Args[0]})
		return
	}

	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantSlug == "" {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	if !ac.SuperAdmin && len(ac.Capabilities) == 0 {
		// Signed non-super-admin with no membership row for this tenant →
		// resolveTenantMiddleware emptied the caps → not a member → denied.
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}

	ctx := clicmd.WithCursor(r.Context(), req.Cursor)
	if cbURL := r.Header.Get("X-Txco-Callback-URL"); cbURL != "" {
		ctx = clicmd.WithCallback(ctx, clicmd.CallbackInfo{URL: cbURL, State: r.Header.Get("X-Txco-Callback-State")})
	}
	res, err := h(ctx, req.Args[1:])
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "command_failed", map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cliExecResponse{
		Stdout:        res.Stdout,
		Stderr:        res.Stderr,
		Exit:          res.Exit,
		Cursor:        res.Cursor,
		PollAfterMs:   res.PollAfterMs,
		OpenURL:       res.OpenURL,
		AwaitCallback: res.AwaitCallback,
	})
}
