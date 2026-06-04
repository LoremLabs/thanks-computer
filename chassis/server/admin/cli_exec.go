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
}

type cliExecResponse struct {
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Exit   int    `json:"exit"`
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

	res, err := h(r.Context(), req.Args[1:])
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "command_failed", map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cliExecResponse{Stdout: res.Stdout, Stderr: res.Stderr, Exit: res.Exit})
}
