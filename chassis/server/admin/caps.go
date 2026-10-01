package admin

// Apply-time gate for CAPS/ declarations (chassis/capdecl), and the read
// of a tenant's catalogue. A declaration that doesn't parse — an unknown
// key, a bad input name — fails the deploy; so does an entry that names no
// scope of the version (or a stack with no scope at all), and a name another ACTIVE stack of the
// tenant already declares: one stack answers a capability, so the inlet's
// lookup by name has one answer. Nothing runs: whether a run may CALL a
// capability is decided when the call is made (chassis/server/grantgw).
// Runs on the HTTP validate and activate paths, like the sandbox gate.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/capdecl"
)

// capBody returns a declaration row's bytes: inline on a single node, from
// the CAS for a fleet row that carries only the fingerprint.
func (c *Controller) capBody(ctx context.Context, f stackFile) ([]byte, error) {
	body := []byte(f.Content)
	if len(body) == 0 && f.ContentHash != "" {
		if c.fcas == nil {
			return nil, fmt.Errorf("declaration bytes are not inline and this chassis has no content store")
		}
		b, err := c.fcas.Get(ctx, f.ContentHash)
		if err != nil {
			return nil, fmt.Errorf("resolve declaration from the CAS: %v", err)
		}
		body = b
	}
	return body, nil
}

// deepValidateCaps parses every CAPS/<name>.yaml in the version, checks
// each entry against the version's scopes, and refuses a name another
// active stack of the tenant declares. Every issue names the path it
// condemns; an empty slice means the version's capabilities are deployable.
func (c *Controller) deepValidateCaps(ctx context.Context, tenantID, stackID string, versionID int64) []datasetIssue {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT path, content, content_hash FROM stack_files
		  WHERE version_id = ? AND (path LIKE ? OR path LIKE '%.txcl') ORDER BY path`),
		versionID, capdecl.Dir+"/%")
	if err != nil {
		return []datasetIssue{{Path: capdecl.Dir + "/", Err: fmt.Sprintf("load capability rows: %v", err)}}
	}
	var decls []stackFile
	scopes := map[int]bool{}
	for rows.Next() {
		var f stackFile
		if err := rows.Scan(&f.Path, &f.Content, &f.ContentHash); err != nil {
			rows.Close()
			return []datasetIssue{{Path: capdecl.Dir + "/", Err: fmt.Sprintf("load capability rows: %v", err)}}
		}
		if capdecl.IsCapPath(f.Path) {
			decls = append(decls, f)
		} else if pf, ok := parseStackPath(f.Path); ok {
			scopes[pf.scope] = true
		}
	}
	rows.Close()
	if len(decls) == 0 {
		return nil
	}

	// What the tenant's OTHER active stacks declare: path → stack name.
	taken := map[string]string{}
	orows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT s.name, sf.path
		        FROM stack_files sf
		        JOIN stacks s ON s.active_version = sf.version_id
		       WHERE s.tenant_id = ? AND s.stack_id <> ? AND sf.path LIKE ?
		       ORDER BY s.name`),
		tenantID, stackID, capdecl.Dir+"/%")
	if err != nil {
		return []datasetIssue{{Path: capdecl.Dir + "/", Err: fmt.Sprintf("load the tenant's capabilities: %v", err)}}
	}
	for orows.Next() {
		var stack, path string
		if err := orows.Scan(&stack, &path); err != nil {
			orows.Close()
			return []datasetIssue{{Path: capdecl.Dir + "/", Err: fmt.Sprintf("load the tenant's capabilities: %v", err)}}
		}
		if _, ok := taken[path]; !ok {
			taken[path] = stack
		}
	}
	orows.Close()

	var issues []datasetIssue
	for _, f := range decls {
		name := capdecl.Name(f.Path)
		if name == "" {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("capability declarations must be a single <name>%s file directly under %s/", capdecl.DeclExt, capdecl.Dir)})
			continue
		}
		body, berr := c.capBody(ctx, f)
		if berr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: berr.Error()})
			continue
		}
		d, perr := capdecl.ParseDecl(body)
		if perr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: perr.Error()})
			continue
		}
		if eerr := capdecl.CheckEntry(d, scopes); eerr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: eerr.Error()})
		}
		if other, dup := taken[f.Path]; dup {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("capability %q is already declared by the active stack %q: one stack answers a capability", name, other)})
		}
	}
	return issues
}

const capIssuesHint = `capabilities are validated before activation: every CAPS/<name>.yaml parses — an optional description, input, timeout and entry (a scope of this stack; omitted, a call enters at the stack's start), nothing else — and no other active stack of the tenant declares the same name`

// capRecord is one capability of a tenant's catalogue.
type capRecord struct {
	Name        string                        `json:"name"`
	Stack       string                        `json:"stack"`
	Entry       int                           `json:"entry"`
	Stage       string                        `json:"stage"`
	Description string                        `json:"description,omitempty"`
	Input       map[string]capdecl.InputField `json:"input,omitempty"`
	Timeout     int                           `json:"timeout,omitempty"`
	// Err is set instead of the declaration's fields when the active row
	// does not parse (it deployed before a rule tightened, or its bytes are
	// missing from the CAS).
	Err string `json:"err,omitempty"`
}

type listCapsResponse struct {
	Caps  []capRecord `json:"caps"`
	Count int         `json:"count"`
}

// handleListCaps: GET /v1/tenants/{t}/caps
//
// The tenant's catalogue: every capability its ACTIVE stacks declare, by
// name, with the stack and scope that answer it. Read-only.
func (c *Controller) handleListCaps(w http.ResponseWriter, r *http.Request) {
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
		       ORDER BY sf.path, s.name`),
		ac.TenantID, capdecl.Dir+"/%")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "caps_list_err", map[string]any{"err": err.Error()})
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
			writeJSONError(w, http.StatusInternalServerError, "caps_list_err", map[string]any{"err": err.Error()})
			return
		}
		found = append(found, rw)
	}
	rows.Close()

	caps := []capRecord{}
	for _, rw := range found {
		name := capdecl.Name(rw.f.Path)
		if name == "" {
			continue
		}
		rec := capRecord{Name: name, Stack: rw.stack}
		body, berr := c.capBody(r.Context(), rw.f)
		if berr != nil {
			rec.Err = berr.Error()
			caps = append(caps, rec)
			continue
		}
		d, perr := capdecl.ParseDecl(body)
		if perr != nil {
			rec.Err = perr.Error()
			caps = append(caps, rec)
			continue
		}
		rec.Entry, rec.Stage = d.Entry, d.Stage(rw.stack)
		rec.Description, rec.Input, rec.Timeout = d.Description, d.Input, d.Timeout
		caps = append(caps, rec)
	}
	writeJSON(w, http.StatusOK, listCapsResponse{Caps: caps, Count: len(caps)})
}
