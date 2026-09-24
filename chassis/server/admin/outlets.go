package admin

// Apply-time gate for OUTLETS/ declarations (chassis/outlet) and the ops
// that call them. Everything here is knowable from stack source, so it
// fails the deploy: a declaration that doesn't parse or names a driver this
// chassis lacks, an EXEC "outlet://..." against an outlet the stack doesn't
// declare, exec against a read outlet, a `sql` that isn't a literal, a
// second statement, the wrong leading verb. Nothing connects — a database
// that is unreachable right now must never block a deploy. Runs on the HTTP
// validate and activate paths, like the dataset deep gate.

import (
	"context"
	"fmt"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// deepValidateOutlets parses every OUTLETS/<name>.yaml in the version, then
// checks every .txcl that EXECs an outlet:// target against them. Every
// issue names the path it condemns; an empty slice means the version's
// outlets are deployable.
func (c *Controller) deepValidateOutlets(ctx context.Context, versionID int64) []datasetIssue {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT path, content, content_hash FROM stack_files
		  WHERE version_id = ? AND (path LIKE ? OR path LIKE '%.txcl') ORDER BY path`),
		versionID, outlet.Dir+"/%")
	if err != nil {
		return []datasetIssue{{Path: outlet.Dir + "/", Err: fmt.Sprintf("load outlet rows: %v", err)}}
	}
	defer rows.Close()
	var decls, ops []stackFile
	for rows.Next() {
		var f stackFile
		if err := rows.Scan(&f.Path, &f.Content, &f.ContentHash); err != nil {
			return []datasetIssue{{Path: outlet.Dir + "/", Err: fmt.Sprintf("load outlet rows: %v", err)}}
		}
		if outlet.IsOutletPath(f.Path) {
			decls = append(decls, f)
		} else {
			ops = append(ops, f)
		}
	}
	var issues []datasetIssue
	declared := map[string]*outlet.Decl{}
	for _, f := range decls {
		name := outlet.Name(f.Path)
		if name == "" {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("outlet declarations must be a single <name>%s file directly under %s/", outlet.DeclExt, outlet.Dir)})
			continue
		}
		body := []byte(f.Content)
		if len(body) == 0 && f.ContentHash != "" {
			// A fleet row carries only the fingerprint; the bytes are in the
			// shared CAS by contract.
			if c.fcas == nil {
				issues = append(issues, datasetIssue{Path: f.Path, Err: "declaration bytes are not inline and this chassis has no content store"})
				continue
			}
			b, gerr := c.fcas.Get(ctx, f.ContentHash)
			if gerr != nil {
				issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("resolve declaration from the CAS: %v", gerr)})
				continue
			}
			body = b
		}
		d, perr := outlet.ParseDecl(body, outlet.Known)
		if perr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: perr.Error()})
			continue
		}
		declared[name] = d
	}
	for _, f := range ops {
		if !strings.Contains(f.Content, outlet.SchemePrefix) {
			continue
		}
		r, perr := txcl.Resonator(f.Content)
		if perr != nil {
			continue // the strict txcl pass reports parse errors
		}
		if !outlet.IsOutletExec(r.Exec) {
			continue
		}
		if cerr := outlet.CheckOp(r.Exec, r.With, declared); cerr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: cerr.Error()})
		}
	}
	return issues
}

// issuesDetail shapes deep-gate issues for a writeJSONError detail.
func issuesDetail(issues []datasetIssue, hint string) map[string]any {
	errs := make([]map[string]any, 0, len(issues))
	for _, i := range issues {
		errs = append(errs, map[string]any{"path": i.Path, "err": i.Err})
	}
	return map[string]any{"errors": errs, "hint": strings.TrimSpace(hint)}
}

const outletIssuesHint = `outlets are validated before activation: every OUTLETS/<name>.yaml parses and names a built-in driver, and every EXEC "outlet://..." names a declared outlet with a literal, single, verb-matched statement`
