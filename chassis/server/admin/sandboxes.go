package admin

// Apply-time gate for SANDBOXES/ declarations (chassis/sandbox). A
// declaration that doesn't parse — an unknown key, no env, a bad variable
// name, a bad reference — fails the deploy. Nothing is released and no
// secret is looked up: whether a principal may be handed what a sandbox
// names is decided when a run grant is minted, not when the file deploys.
// Runs on the HTTP validate and activate paths, like the outlet gate.

import (
	"context"
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// deepValidateSandboxes parses every SANDBOXES/<name>.yaml in the version.
// Every issue names the path it condemns; an empty slice means the
// version's sandboxes are deployable.
func (c *Controller) deepValidateSandboxes(ctx context.Context, versionID int64) []datasetIssue {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT path, content, content_hash FROM stack_files
		  WHERE version_id = ? AND path LIKE ? ORDER BY path`),
		versionID, sandbox.Dir+"/%")
	if err != nil {
		return []datasetIssue{{Path: sandbox.Dir + "/", Err: fmt.Sprintf("load sandbox rows: %v", err)}}
	}
	defer rows.Close()
	var decls []stackFile
	for rows.Next() {
		var f stackFile
		if err := rows.Scan(&f.Path, &f.Content, &f.ContentHash); err != nil {
			return []datasetIssue{{Path: sandbox.Dir + "/", Err: fmt.Sprintf("load sandbox rows: %v", err)}}
		}
		decls = append(decls, f)
	}
	var issues []datasetIssue
	for _, f := range decls {
		if sandbox.Name(f.Path) == "" {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("sandbox declarations must be a single <name>%s file directly under %s/", sandbox.DeclExt, sandbox.Dir)})
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
		if _, perr := sandbox.ParseDecl(body); perr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: perr.Error()})
		}
	}
	return issues
}

const sandboxIssuesHint = `sandboxes are validated before activation: every SANDBOXES/<name>.yaml parses — an env of VARIABLE: secret:NAME pairs, an optional description, nothing else`
