package admin

// Apply-time gates for PROVIDERS/ and CONNECTIONS/ declarations
// (chassis/connection). A provider declaration must parse and its name must
// be no other active stack's: a provider's name is the identity of every
// connection at it, so one active stack answers for it, as for a
// capability. A consumer declaration must parse, name the provider its
// file is named after, and — when the provider is in sight (this version,
// or another active stack) — call only origins the provider's api
// destinations admit. Nothing is connected or looked up: whose token a
// request gets is decided when it is made.

import (
	"context"
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/connection"
)

// deepValidateProviders parses every PROVIDERS/<name>.yaml in the version
// and refuses a name another active stack of the tenant declares.
func (c *Controller) deepValidateProviders(ctx context.Context, tenantID, stackID string, versionID int64) []datasetIssue {
	decls, issues := c.declRows(ctx, versionID, connection.ProviderDir)
	if issues != nil || len(decls) == 0 {
		return issues
	}
	taken, err := c.takenPaths(ctx, tenantID, stackID, connection.ProviderDir)
	if err != nil {
		return []datasetIssue{{Path: connection.ProviderDir + "/", Err: fmt.Sprintf("load the tenant's providers: %v", err)}}
	}
	for _, f := range decls {
		name := connection.ProviderName(f.Path)
		if name == "" {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("provider declarations must be a single <name>%s file directly under %s/, the name matching [a-z][a-z0-9_-]*", connection.DeclExt, connection.ProviderDir)})
			continue
		}
		body, berr := c.capBody(ctx, f)
		if berr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: berr.Error()})
			continue
		}
		if _, perr := connection.ParseProvider(body); perr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: perr.Error()})
			continue
		}
		if other, dup := taken[f.Path]; dup {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("provider %q is already declared by the active stack %q: one stack declares a provider", name, other)})
		}
	}
	return issues
}

// deepValidateConnections parses every CONNECTIONS/<provider>.yaml in the
// version and checks it against the provider when one is in sight.
func (c *Controller) deepValidateConnections(ctx context.Context, tenantID, stackID string, versionID int64) []datasetIssue {
	decls, issues := c.declRows(ctx, versionID, connection.ConsumerDir)
	if issues != nil || len(decls) == 0 {
		return issues
	}
	for _, f := range decls {
		name := connection.ConsumerName(f.Path)
		if name == "" {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("connection declarations must be a single <provider>%s file directly under %s/, the name matching [a-z][a-z0-9_-]*", connection.DeclExt, connection.ConsumerDir)})
			continue
		}
		body, berr := c.capBody(ctx, f)
		if berr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: berr.Error()})
			continue
		}
		cons, perr := connection.ParseConsumer(body)
		if perr != nil {
			issues = append(issues, datasetIssue{Path: f.Path, Err: perr.Error()})
			continue
		}
		if cons.Provider != name {
			issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("the file is for provider %q but `provider` says %q", name, cons.Provider)})
			continue
		}
		prov, where, ok := c.providerInSight(ctx, tenantID, stackID, versionID, name)
		if !ok {
			continue // checked when the connection is used
		}
		for _, o := range cons.OriginList() {
			if !prov.APIOrigin(o) {
				issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("origin %s is not one of provider %q's api destinations (%s)", o, name, where)})
			}
		}
	}
	return issues
}

// declRows is the version's rows under dir.
func (c *Controller) declRows(ctx context.Context, versionID int64, dir string) ([]stackFile, []datasetIssue) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT path, content, content_hash FROM stack_files WHERE version_id = ? AND path LIKE ? ORDER BY path`),
		versionID, dir+"/%")
	if err != nil {
		return nil, []datasetIssue{{Path: dir + "/", Err: fmt.Sprintf("load %s rows: %v", dir, err)}}
	}
	defer rows.Close()
	var decls []stackFile
	for rows.Next() {
		var f stackFile
		if err := rows.Scan(&f.Path, &f.Content, &f.ContentHash); err != nil {
			return nil, []datasetIssue{{Path: dir + "/", Err: fmt.Sprintf("load %s rows: %v", dir, err)}}
		}
		decls = append(decls, f)
	}
	return decls, nil
}

// takenPaths is what the tenant's OTHER active stacks declare under dir:
// path → stack name.
func (c *Controller) takenPaths(ctx context.Context, tenantID, stackID, dir string) (map[string]string, error) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT s.name, sf.path
		        FROM stack_files sf
		        JOIN stacks s ON s.active_version = sf.version_id
		       WHERE s.tenant_id = ? AND s.stack_id <> ? AND sf.path LIKE ?
		       ORDER BY s.name`),
		tenantID, stackID, dir+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	taken := map[string]string{}
	for rows.Next() {
		var stack, path string
		if err := rows.Scan(&stack, &path); err != nil {
			return nil, err
		}
		if _, ok := taken[path]; !ok {
			taken[path] = stack
		}
	}
	return taken, rows.Err()
}

// providerInSight finds the provider a consumer names: in this version
// first, else in another active stack of the tenant. where says which, for
// the message. A declaration that does not parse is not in sight (its own
// gate reports it).
func (c *Controller) providerInSight(ctx context.Context, tenantID, stackID string, versionID int64, name string) (*connection.Provider, string, bool) {
	path := connection.ProviderPath(name)
	var f stackFile
	err := c.pu.RuntimeDB.QueryRowContext(ctx,
		c.rb(`SELECT path, content, content_hash FROM stack_files WHERE version_id = ? AND path = ?`), versionID, path).
		Scan(&f.Path, &f.Content, &f.ContentHash)
	where := "declared in this version"
	if err != nil {
		var stack string
		err = c.pu.RuntimeDB.QueryRowContext(ctx,
			c.rb(`SELECT s.name, sf.path, sf.content, sf.content_hash
			        FROM stack_files sf
			        JOIN stacks s ON s.active_version = sf.version_id
			       WHERE s.tenant_id = ? AND s.stack_id <> ? AND sf.path = ?
			       ORDER BY s.name LIMIT 1`),
			tenantID, stackID, path).Scan(&stack, &f.Path, &f.Content, &f.ContentHash)
		if err != nil {
			return nil, "", false
		}
		where = "declared by the active stack " + stack
	}
	body, berr := c.capBody(ctx, f)
	if berr != nil {
		return nil, "", false
	}
	p, perr := connection.ParseProvider(body)
	if perr != nil {
		return nil, "", false
	}
	return p, where, true
}

const providerIssuesHint = `providers are validated before activation: every PROVIDERS/<name>.yaml parses — kind, destinations (authorization, token, api origins), client_id, client_secret (a secret reference), consumers, and the optional keys — and no other active stack of the tenant declares the same name`

const connectionIssuesHint = `connections are validated before activation: every CONNECTIONS/<provider>.yaml parses — provider (the file's own name), origins, and the optional kind and use — and, when the provider is declared, its origins are among the provider's api destinations`
