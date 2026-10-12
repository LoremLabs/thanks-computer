package admin

// Apply-time gate for CALLS/ declarations (chassis/calldecl), and the read
// of a tenant's declared entries. A declaration that doesn't parse — an
// unknown key, a bad field name — fails the deploy; so does an entry that
// names no scope of the version. Entry names are a stack's own, so nothing
// is checked across stacks. Runs on the HTTP validate and activate paths,
// like the capability gate.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/calldecl"
	"github.com/loremlabs/thanks-computer/chassis/capdecl"
)

// deepValidateCalls parses every CALLS/<name>.yaml in the version and
// checks each entry against the version's scopes. Every issue names the
// path it condemns; an empty slice means the version's entries are
// deployable.
func (c *Controller) deepValidateCalls(ctx context.Context, versionID int64) []datasetIssue {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT path, content, content_hash FROM stack_files
		  WHERE version_id = ? AND (path LIKE ? OR path LIKE '%.txcl') ORDER BY path`),
		versionID, calldecl.Dir+"/%")
	if err != nil {
		return []datasetIssue{{Path: calldecl.Dir + "/", Err: fmt.Sprintf("load call declaration rows: %v", err)}}
	}
	var decls []stackFile
	scopes := map[int]bool{}
	for rows.Next() {
		var f stackFile
		if err := rows.Scan(&f.Path, &f.Content, &f.ContentHash); err != nil {
			rows.Close()
			return []datasetIssue{{Path: calldecl.Dir + "/", Err: fmt.Sprintf("load call declaration rows: %v", err)}}
		}
		if calldecl.IsCallPath(f.Path) {
			decls = append(decls, f)
		} else if pf, ok := parseStackPath(f.Path); ok {
			scopes[pf.scope] = true
		}
	}
	rows.Close()
	if len(decls) == 0 {
		return nil
	}

	var issues []datasetIssue
	for _, f := range decls {
		if calldecl.Name(f.Path) == "" {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("call declarations must be a single <name>%s file directly under %s/", calldecl.DeclExt, calldecl.Dir)})
			continue
		}
		body, berr := c.capBody(ctx, f)
		if berr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: berr.Error()})
			continue
		}
		d, perr := calldecl.ParseDecl(body)
		if perr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: perr.Error()})
			continue
		}
		if eerr := calldecl.CheckEntry(d, scopes); eerr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: eerr.Error()})
		}
	}
	return issues
}

const callIssuesHint = `call entries are validated before activation: every CALLS/<name>.yaml parses — an optional description, input, output, timeout and entry (a scope of this stack; omitted, a call enters at the stack's start), nothing else`

// callRecord is one declared entry of a tenant's stacks.
type callRecord struct {
	Stack       string                   `json:"stack"`
	Name        string                   `json:"name"`
	Entry       int                      `json:"entry"`
	Stage       string                   `json:"stage"`
	Description string                   `json:"description,omitempty"`
	Input       map[string]capdecl.Field `json:"input,omitempty"`
	Output      map[string]capdecl.Field `json:"output,omitempty"`
	Timeout     int                      `json:"timeout,omitempty"`
	// Err is set instead of the declaration's fields when the active row
	// does not parse.
	Err string `json:"err,omitempty"`
}

type listCallsResponse struct {
	Calls []callRecord `json:"calls"`
	Count int          `json:"count"`
}

// handleListCalls: GET /v1/tenants/{t}/calls
//
// Every entry the tenant's ACTIVE stacks declare under CALLS/, by stack and
// name, with where a call enters and what it takes and answers. Read-only.
func (c *Controller) handleListCalls(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	rows, err := c.pu.RuntimeDB.QueryContext(r.Context(),
		c.rb(`SELECT s.name, sf.path, sf.content, sf.content_hash
		        FROM stack_files sf
		        JOIN stacks s ON s.active_version = sf.version_id
		       WHERE s.tenant_id = ? AND sf.path LIKE ?
		       ORDER BY s.name, sf.path`),
		ac.TenantID, calldecl.Dir+"/%")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "calls_list_err", map[string]any{"err": err.Error()})
		return
	}
	type row struct {
		stack string
		f     stackFile
	}
	var found []row
	for rows.Next() {
		var rw row
		if err := rows.Scan(&rw.stack, &rw.f.Path, &rw.f.Content, &rw.f.ContentHash); err != nil {
			rows.Close()
			writeJSONError(w, http.StatusInternalServerError, "calls_list_err", map[string]any{"err": err.Error()})
			return
		}
		found = append(found, rw)
	}
	rows.Close()

	calls := []callRecord{}
	for _, rw := range found {
		name := calldecl.Name(rw.f.Path)
		if name == "" {
			continue
		}
		rec := callRecord{Stack: rw.stack, Name: name}
		body, berr := c.capBody(r.Context(), rw.f)
		if berr != nil {
			rec.Err = berr.Error()
			calls = append(calls, rec)
			continue
		}
		d, perr := calldecl.ParseDecl(body)
		if perr != nil {
			rec.Err = perr.Error()
			calls = append(calls, rec)
			continue
		}
		rec.Entry, rec.Stage = d.Entry, d.Stage(rw.stack)
		rec.Description, rec.Input, rec.Output, rec.Timeout = d.Description, d.Input, d.Output, d.Timeout
		calls = append(calls, rec)
	}
	writeJSON(w, http.StatusOK, listCallsResponse{Calls: calls, Count: len(calls)})
}
