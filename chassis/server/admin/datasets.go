package admin

// Apply-time gate for DATASETS/ artifact+manifest pairs (chassis/dataset).
//
// Split by cost, mirroring how FILES avoid stalling the applier:
//   - PAIR completeness (paths only, no bytes) runs inside
//     materialiseStackVersion on every path — origin, single-node, and the
//     fleet applier alike.
//   - The DEEP gate (artifact present in the CAS, manifest parses, artifact
//     opens, every declared query prepares under the read-only authorizer)
//     needs artifact bytes local, which can mean a multi-GB CAS fetch. It
//     runs ONLY on the HTTP validate/activate paths — never inside the
//     applier's tx, where a long fetch would pin the runtime DB connection
//     (the materialiseFiles fleet-gate lesson). Data-plane nodes apply
//     versions the origin already gated, and warm their caches post-commit.

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/dataset"
)

// SetDatasetCache wires the node's dataset artifact cache (chassis/dataset).
// Set by chassis/server/server.go at boot. Nil-safe: when unset, activating
// a dataset-bearing version fails loudly rather than shipping queries that
// can never run.
func (c *Controller) SetDatasetCache(dc *dataset.Cache) { c.dsCache = dc }

// datasetPairError enforces DATASETS/ completeness over a version's paths:
// every <name>.sqlite needs its <name>.yaml and vice versa — an artifact
// without queries is unreachable, a manifest without bytes is a lie.
func datasetPairError(paths []string) *materialiseError {
	type pair struct{ artifact, manifest bool }
	pairs := map[string]*pair{}
	get := func(n string) *pair {
		if p, ok := pairs[n]; ok {
			return p
		}
		p := &pair{}
		pairs[n] = p
		return p
	}
	for _, p := range paths {
		switch {
		case dataset.IsArtifactPath(p):
			get(dataset.Name(p)).artifact = true
		case dataset.IsManifestPath(p):
			get(dataset.Name(p)).manifest = true
		}
	}
	var missing []string
	for n, p := range pairs {
		switch {
		case !p.manifest:
			missing = append(missing, dataset.Dir+"/"+n+dataset.ManifestExt)
		case !p.artifact:
			missing = append(missing, dataset.Dir+"/"+n+dataset.ArtifactExt)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return &materialiseError{http.StatusUnprocessableEntity, "dataset_pair_incomplete", map[string]any{
		"missing": missing,
		"hint":    "each DATASETS/<name>.sqlite ships with a DATASETS/<name>.yaml of named queries (and vice versa)",
	}}
}

// datasetMember is one artifact+manifest pair resolved from a version's rows.
type datasetMember struct {
	Name         string
	ArtifactHash string
	ManifestBody []byte
}

// datasetIssue is one deep-gate failure, shaped for the validate response.
type datasetIssue struct {
	Path string
	Err  string
}

// datasetMembersFromFiles pairs up a version's DATASETS/ rows, resolving
// manifest bytes from the row (inline) or the CAS (fingerprint-only rows on
// fleet nodes). Rows outside DATASETS/ are ignored, so callers can pass the
// whole file set.
func (c *Controller) datasetMembersFromFiles(ctx context.Context, files []stackFile) ([]datasetMember, []datasetIssue) {
	byName := map[string]*datasetMember{}
	var issues []datasetIssue
	get := func(n string) *datasetMember {
		if m, ok := byName[n]; ok {
			return m
		}
		m := &datasetMember{Name: n}
		byName[n] = m
		return m
	}
	for _, f := range files {
		switch {
		case dataset.IsArtifactPath(f.Path):
			hash := f.ContentHash
			if hash == "" && f.Content != "" {
				hash = sha256Hex(f.Content)
			}
			get(dataset.Name(f.Path)).ArtifactHash = hash
		case dataset.IsManifestPath(f.Path):
			m := get(dataset.Name(f.Path))
			if f.Content != "" {
				m.ManifestBody = []byte(f.Content)
				continue
			}
			if f.ContentHash == "" {
				issues = append(issues, datasetIssue{Path: f.Path, Err: "manifest row has neither content nor content_hash"})
				continue
			}
			if c.fcas == nil {
				issues = append(issues, datasetIssue{Path: f.Path, Err: "no filecas store configured; cannot resolve manifest bytes"})
				continue
			}
			body, err := c.fcas.Get(ctx, f.ContentHash)
			if err != nil {
				issues = append(issues, datasetIssue{Path: f.Path, Err: fmt.Sprintf("resolve manifest from CAS: %v", err)})
				continue
			}
			m.ManifestBody = body
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	members := make([]datasetMember, 0, len(names))
	for _, n := range names {
		members = append(members, *byName[n])
	}
	return members, issues
}

// validateDatasetMembers is the deep gate. Every returned issue names the
// member path it condemns; an empty slice means the version's datasets are
// deployable. The artifact size cap is enforced where bytes enter (the blob
// endpoint / draft PUT), not re-measured here.
func (c *Controller) validateDatasetMembers(ctx context.Context, members []datasetMember) []datasetIssue {
	var issues []datasetIssue
	for _, m := range members {
		artifactPath := dataset.Dir + "/" + m.Name + dataset.ArtifactExt
		manifestPath := dataset.Dir + "/" + m.Name + dataset.ManifestExt
		if m.ArtifactHash == "" || m.ManifestBody == nil {
			// Pair incompleteness is reported by datasetPairError; a member
			// missing bytes here (unresolvable manifest) was already issued.
			continue
		}
		manifest, err := dataset.ParseManifest(m.ManifestBody)
		if err != nil {
			issues = append(issues, datasetIssue{Path: manifestPath, Err: err.Error()})
			continue
		}
		if c.fcas == nil || c.dsCache == nil {
			issues = append(issues, datasetIssue{Path: artifactPath, Err: "this chassis has no dataset store; dataset-bearing stacks cannot activate here"})
			continue
		}
		ok, err := c.fcas.Exists(ctx, m.ArtifactHash)
		if err != nil {
			issues = append(issues, datasetIssue{Path: artifactPath, Err: fmt.Sprintf("CAS presence check: %v", err)})
			continue
		}
		if !ok {
			issues = append(issues, datasetIssue{Path: artifactPath,
				Err: fmt.Sprintf("artifact %s not in the CAS — stream it to PUT /blobs/sha256/{hash} (txco apply does this) before activating", m.ArtifactHash)})
			continue
		}
		local, err := c.dsCache.LocalPath(ctx, m.ArtifactHash)
		if err != nil {
			issues = append(issues, datasetIssue{Path: artifactPath, Err: fmt.Sprintf("materialise artifact: %v", err)})
			continue
		}
		if err := dataset.ValidateArtifact(ctx, local, manifest); err != nil {
			// Named for the failing query by ValidateArtifact; a write/DDL
			// statement fails preparation under the read-only authorizer.
			issues = append(issues, datasetIssue{Path: manifestPath, Err: err.Error()})
		}
	}
	return issues
}

// deepValidateDatasets loads a version's dataset members and runs the deep
// gate; used by the HTTP validate + activate paths. It reads the DATASETS/
// rows directly — NOT via loadVersionFiles(contentAll), which CAS-resolves
// every fingerprint row into memory (buffering the multi-GB artifact this
// gate exists to avoid touching) and hard-fails on the missing-blob case
// the gate wants to report per-path.
func (c *Controller) deepValidateDatasets(ctx context.Context, versionID int64) []datasetIssue {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT path, content, content_hash FROM stack_files
		  WHERE version_id = ? AND path LIKE ? ORDER BY path`),
		versionID, dataset.Dir+"/%")
	if err != nil {
		return []datasetIssue{{Path: dataset.Dir + "/", Err: fmt.Sprintf("load dataset rows: %v", err)}}
	}
	defer rows.Close()
	var files []stackFile
	for rows.Next() {
		var f stackFile
		if err := rows.Scan(&f.Path, &f.Content, &f.ContentHash); err != nil {
			return []datasetIssue{{Path: dataset.Dir + "/", Err: fmt.Sprintf("load dataset rows: %v", err)}}
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil
	}
	members, issues := c.datasetMembersFromFiles(ctx, files)
	return append(issues, c.validateDatasetMembers(ctx, members)...)
}

// WarmDatasets prefetches + opens every dataset artifact the given stack
// version references, so a data-plane node's first query doesn't eat a
// multi-GB CAS fetch inside a request. Post-commit, best-effort: failures
// log and the lazy path remains (the op materialises on demand).
func (c *Controller) WarmDatasets(ctx context.Context, tenantID, stackName string, versionNumber int64) {
	if c.dsCache == nil || c.fcas == nil {
		return
	}
	rows, err := c.pu.RuntimeDB.QueryContext(ctx, c.rb(`
		SELECT sf.path, sf.content_hash
		  FROM stack_files sf
		  JOIN stack_versions sv ON sf.version_id = sv.version_id
		  JOIN stacks s          ON sv.stack_id = s.stack_id
		 WHERE s.tenant_id = ? AND s.name = ? AND sv.version_number = ?
		   AND sf.path LIKE ?`),
		tenantID, stackName, versionNumber, dataset.Dir+"/%")
	if err != nil {
		c.pu.Logger.Warn("dataset warm: list members", zap.String("stack", stackName), zap.Error(err))
		return
	}
	type member struct{ path, hash string }
	var artifacts []member
	for rows.Next() {
		var m member
		if rows.Scan(&m.path, &m.hash) == nil && dataset.IsArtifactPath(m.path) && m.hash != "" {
			artifacts = append(artifacts, m)
		}
	}
	_ = rows.Close()
	for _, a := range artifacts {
		if _, err := c.dsCache.Handle(ctx, a.hash); err != nil {
			c.pu.Logger.Warn("dataset warm: materialise failed; first query will retry lazily",
				zap.String("stack", stackName), zap.String("path", a.path),
				zap.String("hash", a.hash), zap.Error(err))
		}
	}
}

// datasetIssuesDetail shapes deep-gate issues for a writeJSONError detail.
func datasetIssuesDetail(issues []datasetIssue) map[string]any {
	errs := make([]map[string]any, 0, len(issues))
	for _, i := range issues {
		errs = append(errs, map[string]any{"path": i.Path, "err": i.Err})
	}
	return map[string]any{"errors": errs, "hint": strings.TrimSpace(`
datasets are validated before activation: artifact uploaded, manifest parses, every query prepares read-only against the shipped schema`)}
}
