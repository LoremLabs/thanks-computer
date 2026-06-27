package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/cli/oprefs"
	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/opname"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// Stack-level types serialised on the wire. version_number is the
// per-stack counter that users see in URLs, CLIs, and UIs;
// version_id is internal and never surfaced.

type stackRecord struct {
	Name          string `json:"name"`
	ActiveVersion *int64 `json:"active_version,omitempty"` // version_number, nil if no active version
	ManifestHash  string `json:"manifest_hash,omitempty"`  // active version's manifest hash (ALL files); lets a client skip a no-op push
	// CodeManifestHash is the active version's manifest over CODE files only
	// (rules + FILES, excluding the VECTORS/+KV/ store-seed packs). Because
	// `txco apply` is code-only and a dev may not have the packs locally, the
	// no-op short-circuit compares the local code manifest to THIS, not the
	// all-files ManifestHash (which a pack-bearing stack would never match).
	CodeManifestHash string `json:"code_manifest_hash,omitempty"`
	CreatedAt        string `json:"created_at"`
	MintHostname     bool   `json:"mint_hostname"` // false = headless (no auto-minted routing URL); see `txco stack set --no-host`
}

type versionRecord struct {
	VersionNumber int64   `json:"version_number"`
	Status        string  `json:"status"`
	ParentVersion *int64  `json:"parent_version_number,omitempty"`
	CreatedBy     string  `json:"created_by"`
	CreatedAt     string  `json:"created_at"`
	ActivatedAt   *string `json:"activated_at,omitempty"`
	ManifestHash  string  `json:"manifest_hash"`
	IsActive      bool    `json:"is_active"`
}

type stackFile struct {
	Path        string `json:"path"`
	Content     string `json:"content,omitempty"`
	ContentHash string `json:"content_hash"`
	// Encoding is "base64" when Content is base64-encoded (non-UTF-8 binary
	// assets the JSON wire would mangle). Decoded to raw bytes before
	// hashing/storing; empty = raw UTF-8 text.
	Encoding string `json:"encoding,omitempty"`
}

type versionDetail struct {
	versionRecord
	Files []stackFile `json:"files"`
}

type listStacksResponse struct {
	Stacks []stackRecord `json:"stacks"`
}

type listVersionsResponse struct {
	Versions []versionRecord `json:"versions"`
}

type createDraftRequest struct {
	// From: "active" (clone the currently-active version's files) or a
	// specific version_number to clone from. Empty/missing → empty draft.
	From string `json:"from,omitempty"`
}

type createDraftResponse struct {
	VersionNumber int64 `json:"version_number"`
}

type patchStackSettingsRequest struct {
	// MintHostname sets the per-stack auto-URL gate (stacks.mint_hostname).
	// Pointer so an omitted field is a no-op rather than a silent reset:
	// nil → 400 (nothing to change); false → headless; true → re-enable.
	MintHostname *bool `json:"mint_hostname,omitempty"`
	// Force authorizes the destructive part of going headless: revoking any
	// live chassis-minted routing host already bound to the stack. Without it,
	// a --no-host on a stack that already has a URL is a 409 (the URL would
	// otherwise keep serving — going headless wouldn't take effect).
	Force bool `json:"force,omitempty"`
}

type stackSettingsResponse struct {
	MintHostname bool `json:"mint_hostname"`
	// RevokedHosts lists the routing hostnames torn down by a forced --no-host
	// (empty when none existed or the stack was already headless).
	RevokedHosts []string `json:"revoked_hosts,omitempty"`
}

type putFilesRequest struct {
	Files []stackFile `json:"files"`
	// Manage selects which file categories this upload is authoritative for; the
	// rest are PRESERVED from the cloned draft. Data is opt-in: `txco apply`
	// sends "code" (replace rules + FILES, carry store-seed packs forward);
	// `txco data apply` sends "data" (replace VECTORS/+KV/ packs, carry code
	// forward). "" / "all" replaces everything — the historical full-deploy
	// behavior, used by `txco dev` and any pre-flag client.
	Manage string `json:"manage,omitempty"`
}

type putFilesResponse struct {
	ManifestHash string `json:"manifest_hash"`
}

type activateRequest struct {
	VersionNumber int64 `json:"version_number"`
}

type activateResponse struct {
	PriorVersionNumber *int64 `json:"prior_version_number,omitempty"`
	VersionNumber      int64  `json:"version_number"`
	// StructuredURL is the auto-minted reachable URL for the stack
	// (omitted when --structured-host-suffix is unset or the stack is
	// a system stack). Scheme/port are derived from the request, not
	// hardcoded.
	StructuredURL string `json:"structured_url,omitempty"`
}

type validateError struct {
	Path string `json:"path"`
	Err  string `json:"err"`
}

type validateResponse struct {
	OK      bool            `json:"ok"`
	Errors  []validateError `json:"errors,omitempty"`
	Checked int             `json:"checked"`
}

type diffEntry struct {
	Path     string `json:"path"`
	Change   string `json:"change"` // "added" | "changed" | "removed"
	FromHash string `json:"from_hash,omitempty"`
	ToHash   string `json:"to_hash,omitempty"`
}

type diffResponse struct {
	V1      int64       `json:"v1"`
	V2      int64       `json:"v2"`
	Equal   bool        `json:"equal"`
	Entries []diffEntry `json:"entries,omitempty"`
}

type patchFileRequest struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	BaseHash string `json:"base_hash"`
}

type patchFileResponse struct {
	Path         string `json:"path"`
	ContentHash  string `json:"content_hash"`
	ManifestHash string `json:"manifest_hash"`
}

type deleteFileRequest struct {
	Path     string `json:"path"`
	BaseHash string `json:"base_hash"`
}

type deleteFileResponse struct {
	Path         string `json:"path"`
	Deleted      bool   `json:"deleted"`
	ManifestHash string `json:"manifest_hash"`
}

// --- helpers ---------------------------------------------------------

// lookupStack returns the stack row for (tenantID, name). Returns
// ErrNoRows if no such stack exists.
func (c *Controller) lookupStack(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID, name string) (stackID string, activeVersionID sql.NullInt64, err error) {
	row := tx.QueryRowContext(ctx,
		`SELECT stack_id, active_version FROM stacks WHERE tenant_id = ? AND name = ?`,
		tenantID, name)
	err = row.Scan(&stackID, &activeVersionID)
	return
}

// stackMintsHost reports whether the stack's auto-URL mint is enabled
// (stacks.mint_hostname). A stack toggled headless via `txco stack set
// --no-host` returns false, suppressing the structured/zone hostname mint at
// activate. Defaults to true when the row or column is absent, or on any read
// error — minting is the historical default, so a missing column or a transient
// read must never silently drop a stack's URL. Only the control-plane activate
// path calls this (gated behind mintHosts), so the data-plane applier adds no
// query.
func (c *Controller) stackMintsHost(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, tenantID, name string) bool {
	var v sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT mint_hostname FROM stacks WHERE tenant_id = ? AND name = ?`,
		tenantID, name).Scan(&v); err != nil {
		return true
	}
	return !v.Valid || v.Int64 != 0
}

// lookupVersion finds (stack_id, version_number) → version_id + status.
func (c *Controller) lookupVersion(ctx context.Context, tx interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, stackID string, versionNumber int64) (versionID int64, status string, err error) {
	row := tx.QueryRowContext(ctx,
		`SELECT version_id, status FROM stack_versions WHERE stack_id = ? AND version_number = ?`,
		stackID, versionNumber)
	err = row.Scan(&versionID, &status)
	return
}

// versionNumberFor maps a version_id back to its (stack_id, version_number).
// Used to translate internal pointers (e.g. stacks.active_version) into
// the wire-visible per-stack counter.
func (c *Controller) versionNumberFor(ctx context.Context, versionID int64) (int64, error) {
	var n int64
	err := c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT version_number FROM stack_versions WHERE version_id = ?`, versionID).Scan(&n)
	return n, err
}

// computeManifestHash hashes (sorted path + content_hash) pairs into
// one hex digest. Stable for a given file set regardless of insert
// order. Empty file sets hash to sha256("").
func computeManifestHash(files []stackFile) string {
	sorted := make([]stackFile, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		h.Write([]byte(f.Path))
		h.Write([]byte{0})
		h.Write([]byte(f.ContentHash))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// codeManifestHash computes the manifest over the stack's ACTIVE version CODE
// files (rules + FILES), excluding the VECTORS/+KV/ store-seed packs. It is the
// basis the code-only `txco apply` no-op short-circuit compares against — a
// pack-bearing stack's all-files ManifestHash would never match a code-only
// local file set, so without this every code re-apply would needlessly re-mint a
// version. Returns "" (no error) when the stack has no active version.
func (c *Controller) codeManifestHash(ctx context.Context, tenantID, name string) (string, error) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx, `
		SELECT sf.path, sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks s ON s.active_version = sf.version_id
		 WHERE s.tenant_id = ? AND s.name = ?
		   AND sf.path NOT LIKE 'VECTORS/%' AND sf.path NOT LIKE 'KV/%'`,
		tenantID, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var files []stackFile
	any := false
	for rows.Next() {
		any = true
		var f stackFile
		var content string
		if err := rows.Scan(&f.Path, &content, &f.ContentHash); err != nil {
			return "", err
		}
		if f.ContentHash == "" {
			f.ContentHash = sha256Hex(content)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !any {
		return "", nil // no active version (or no files) → no code manifest to compare
	}
	return computeManifestHash(files), nil
}

// validateStackFilePath enforces the same rules at every endpoint that
// writes stack_files. Paths get materialised to disk by `txco pull`,
// so they must be safe relative paths: no absolute form, no `..`/`.`
// segments, no empty segments, and the caller's spelling must equal
// `path.Clean(p)`. Extensions are whitelisted: `.txcl` for rule
// bodies; `.json` only for `mock-request.json` / `mock-response.json`
// (the two well-known mock fixture filenames the activate handler
// recognises). Anything else lands as dead weight that wouldn't
// materialise anyway.
//
// Returns nil on success, or an error whose message goes verbatim
// into the 400 response body's `detail.reason`.
// validateStackName guards the stack namespace the data plane treats
// specially. `boot/*` (and bare `boot`) is the chassis-wide unrouted
// fallback: a request matching no ingress route dispatches to the
// untenanted stage `boot/%/0` (chassis/server.defaultEntryStage), whose
// op lookup is deliberately NOT tenant-filtered
// (processor.lookupOpsExact with an empty tenant scope). If a tenant
// could own a `boot/<x>` stack, its scope-0 rules would be swept into
// that fallback via `stack LIKE 'boot/%'` and execute for traffic that
// isn't theirs — a cross-tenant escalation. The comparison is
// case-insensitive because SQLite's LIKE is ASCII-case-insensitive, so
// `BOOT/x` would still match the `boot/%` pattern. `%` is rejected
// outright: it is the LIKE wildcard and has no legitimate use in a
// stack name (a name containing it would itself behave as a wildcard in
// the op lookup).
//
// boot/* is owned exclusively by the reserved system tenant: a request
// that matches no ingress route runs pinned to `_sys` against the
// `boot/%` namespace, so only `_sys`-owned ops may live there.
// actingTenantID is the tenant the caller is operating as
// (auth.FromContext .TenantID); boot/* is permitted iff that is the
// system tenant. Every other tenant is rejected, which is what keeps a
// tenant from injecting rules into the chassis-wide fallback.
//
// Returns nil on success, or an error whose message goes verbatim into
// the 400 response body's `detail.reason`.
func validateStackName(name, actingTenantID string) error {
	lower := strings.ToLower(name)
	if lower == "boot" || strings.HasPrefix(lower, "boot/") {
		if actingTenantID != tenants.SystemTenantID {
			return fmt.Errorf("stack name %q is reserved: boot/* is owned by the system tenant (the chassis ingress-fallback namespace)", name)
		}
	}
	// Charset/segment rule (also bans '%', '.'/'..', whitespace,
	// leading/trailing or doubled '/'). The boot/* reservation above is
	// policy and stays here; the shape rule is shared via opname.
	if err := opname.ValidStack(name); err != nil {
		return err
	}
	return nil
}

func validateStackFilePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must be relative, not absolute")
	}
	if p != path.Clean(p) {
		return fmt.Errorf("path must be normalised (no '.', '..', '//', or trailing '/')")
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("empty path segment")
		case ".", "..":
			return fmt.Errorf("'.' and '..' segments are not allowed")
		}
	}
	// FILES/** are tenant static assets served by txco://static (or read by
	// _*-convention ops). Any extension is allowed — the segment/clean checks
	// above already reject traversal and dotfiles; content-type is derived at
	// serve time, not from the name. Their bytes are content-addressed in the
	// filecas store on activation, not materialised into ops.
	if strings.HasPrefix(p, "FILES/") {
		return nil
	}

	// VECTORS/** and KV/** are declarative store-seed packs (NDJSON),
	// reconciled into the vector / KV stores on activation rather than served
	// or materialised into ops (see chassis/storeseed). Their bytes are
	// content-addressed like FILES/. We require a single name segment with the
	// .jsonl extension — "VECTORS/<collection>.jsonl", "KV/<namespace>.jsonl" —
	// so the collection/namespace each pack owns is unambiguous (no nesting).
	if storeseed.KindForPath(p) != "" {
		if storeseed.PackName(p) == "" {
			return fmt.Errorf("store-seed packs must be a single %q file directly under %s/ or %s/ (got %q)",
				storeseed.PackExt, storeseed.DirVectors, storeseed.DirKV, p)
		}
		return nil
	}

	switch {
	case strings.HasSuffix(p, ".txcl"):
		// The rule name is the .txcl stem after the leading "<scope>/"
		// (exactly what parseStackPath derives into ops.name). Reject a
		// bad name here, at the write boundary, instead of silently at
		// activate. `_legacy_*` is the sentinel for an unnamed rule
		// (name=''), which is legitimate and skipped.
		if m := pathRe.FindStringSubmatch(p); m != nil {
			stem := strings.TrimSuffix(m[2], ".txcl")
			if !strings.HasPrefix(stem, "_legacy_") {
				if err := opname.Valid(stem); err != nil {
					return err
				}
			}
		}
	case strings.HasSuffix(p, ".json"):
		base := path.Base(p)
		if base != "mock-request.json" && base != "mock-response.json" {
			return fmt.Errorf(".json files must be named mock-request.json or mock-response.json")
		}
	default:
		return fmt.Errorf("unsupported extension; only .txcl and the two mock-*.json fixtures are allowed")
	}
	return nil
}

// recomputeManifestHash reads every (path, content_hash) for the
// version inside the supplied transaction, falls back to hashing the
// live content when a row's stored hash is empty (backfilled rows),
// computes the manifest hash via the existing helper, and writes it
// back to stack_versions. Used at the tail of PATCH and DELETE so
// the diff endpoint's manifest-hash short-circuit stays consistent.
func (c *Controller) recomputeManifestHash(ctx context.Context, tx *sql.Tx, versionID int64) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT path, content, content_hash FROM stack_files WHERE version_id = ? ORDER BY path`,
		versionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var files []stackFile
	for rows.Next() {
		var f stackFile
		var content string
		if err := rows.Scan(&f.Path, &content, &f.ContentHash); err != nil {
			return "", err
		}
		if f.ContentHash == "" {
			f.ContentHash = sha256Hex(content)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	mhash := computeManifestHash(files)
	if _, err := tx.ExecContext(ctx,
		`UPDATE stack_versions SET manifest_hash = ? WHERE version_id = ?`,
		mhash, versionID); err != nil {
		return "", err
	}
	return mhash, nil
}

// --- handlers --------------------------------------------------------

// handleListStacks: GET /v1/tenants/{t}/stacks
func (c *Controller) handleListStacks(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	rows, err := c.pu.RuntimeDB.QueryContext(r.Context(),
		`SELECT s.name, sv.version_number, sv.manifest_hash, s.created_at, s.mint_hostname
		   FROM stacks s
		   LEFT JOIN stack_versions sv ON sv.version_id = s.active_version
		  WHERE s.tenant_id = ?
		  ORDER BY s.name`, ac.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	defer rows.Close()
	var out []stackRecord
	for rows.Next() {
		var rec stackRecord
		var av sql.NullInt64
		var mh sql.NullString
		var mint int64
		if err := rows.Scan(&rec.Name, &av, &mh, &rec.CreatedAt, &mint); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "scan_failed", map[string]any{"err": err.Error()})
			return
		}
		if av.Valid {
			rec.ActiveVersion = &av.Int64
		}
		rec.ManifestHash = mh.String
		rec.MintHostname = mint != 0
		out = append(out, rec)
	}
	writeJSON(w, http.StatusOK, listStacksResponse{Stacks: out})
}

// handleGetStack: GET /v1/tenants/{t}/stacks/{name}
func (c *Controller) handleGetStack(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	name := mux.Vars(r)["name"]
	var rec stackRecord
	rec.Name = name
	var av sql.NullInt64
	var mh sql.NullString
	var mint int64
	err := c.pu.RuntimeDB.QueryRowContext(r.Context(),
		`SELECT sv.version_number, sv.manifest_hash, s.created_at, s.mint_hostname
		   FROM stacks s
		   LEFT JOIN stack_versions sv ON sv.version_id = s.active_version
		  WHERE s.tenant_id = ? AND s.name = ?`, ac.TenantID, name).Scan(&av, &mh, &rec.CreatedAt, &mint)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	if av.Valid {
		rec.ActiveVersion = &av.Int64
		// Code-only manifest for the no-op short-circuit (data is opt-in; the
		// client deploys CODE and compares against this, not the all-files hash).
		// Best-effort: a compute error just omits it, falling back to a deploy.
		if cmh, cerr := c.codeManifestHash(r.Context(), ac.TenantID, name); cerr == nil {
			rec.CodeManifestHash = cmh
		}
	}
	rec.ManifestHash = mh.String
	rec.MintHostname = mint != 0
	writeJSON(w, http.StatusOK, rec)
}

// handlePatchStackSettings: PATCH /v1/tenants/{t}/stacks/{name}/settings
//
// Updates stack-level policy that lives on the stacks row itself (not on a
// version). Currently just mint_hostname — the per-stack auto-URL gate flipped
// by `txco stack set --no-host|--host`. Vivifies the stacks row if it doesn't
// exist yet (mirrors handleCreateDraft), so the toggle can be set BEFORE a
// stack's first apply — otherwise the URL would already be minted by the time
// you could flip it. Suppress-future-mint only: it never revokes a live host.
func (c *Controller) handlePatchStackSettings(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	name := mux.Vars(r)["name"]
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "empty_stack_name", nil)
		return
	}
	if err := validateStackName(name, ac.TenantID); err != nil {
		writeJSONError(w, http.StatusBadRequest, "reserved_stack_name", map[string]any{"reason": err.Error()})
		return
	}
	var req patchStackSettingsRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
			return
		}
	}
	if req.MintHostname == nil {
		writeJSONError(w, http.StatusBadRequest, "no_settings", map[string]any{"reason": "no settings provided (expected mint_hostname)"})
		return
	}
	goingHeadless := !*req.MintHostname

	// Going headless on a stack that already has a live chassis-minted URL
	// must tear that URL down to take effect (the column only gates FUTURE
	// mints; the existing host keeps routing otherwise). That is destructive
	// and outward-facing, so it requires --force; without it, refuse with the
	// live host(s) named so the operator can decide. The hostname revoke is a
	// distinct capability, so an actor force-revoking must hold it too.
	var toRevoke []tenants.Hostname
	if goingHeadless {
		hosts, herr := c.listManagedStackHosts(r.Context(), ac.TenantID, name)
		if herr != nil {
			writeJSONError(w, http.StatusInternalServerError, "lookup_hosts", map[string]any{"err": herr.Error()})
			return
		}
		if len(hosts) > 0 {
			if !req.Force {
				names := make([]string, len(hosts))
				for i, h := range hosts {
					names[i] = h.Hostname
				}
				writeJSONError(w, http.StatusConflict, "live_url_exists", map[string]any{
					"hostnames": names,
					"hint":      "stack already has a live URL; re-run with --force to revoke it and go headless",
				})
				return
			}
			if err := policy.RequireCapability(r.Context(), "hostname:*:write"); err != nil {
				auth.WriteForbidden(w, signature.ErrCapabilityDenied)
				return
			}
			toRevoke = hosts
		}
	}

	nowT := time.Now().UTC()
	now := nowT.Format("2006-01-02T15:04:05.000Z")

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Look up or auto-vivify the stack (mirrors handleCreateDraft).
	var stackID string
	var activeVersionID sql.NullInt64
	switch err = tx.QueryRowContext(r.Context(),
		`SELECT stack_id, active_version FROM stacks WHERE tenant_id = ? AND name = ?`,
		ac.TenantID, name).Scan(&stackID, &activeVersionID); {
	case errors.Is(err, sql.ErrNoRows):
		stackID = "stk_" + hxid.NewTimeSort().String()
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES (?, ?, ?, ?)`,
			stackID, ac.TenantID, name, now); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "create_stack", map[string]any{"err": err.Error()})
			return
		}
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_stack", map[string]any{"err": err.Error()})
		return
	}

	mint := 0
	if *req.MintHostname {
		mint = 1
	}
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE stacks SET mint_hostname = ? WHERE tenant_id = ? AND name = ?`,
		mint, ac.TenantID, name); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "update_settings", map[string]any{"err": err.Error()})
		return
	}

	revoked := make([]string, 0, len(toRevoke))
	for _, h := range toRevoke {
		if err := c.revokeManagedHostTx(r.Context(), tx, ac.TenantID, h, nowT); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "revoke_host", map[string]any{"err": err.Error(), "hostname": h.Hostname})
			return
		}
		revoked = append(revoked, h.Hostname)
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	// A revoke changes data-plane routing (the resolver reads tenant_hostnames),
	// so refresh the cache like handleRevokeHostname does. The column flip alone
	// needs no reload (mint_hostname is read only at activate).
	if len(revoked) > 0 {
		if err := c.pu.Dbc.Reload(); err != nil {
			c.pu.Logger.Warn("dbcache reload after stack-headless revoke failed; FS watcher will retry",
				zap.String("err", err.Error()))
		}
	}

	writeJSON(w, http.StatusOK, stackSettingsResponse{MintHostname: mint != 0, RevokedHosts: revoked})
}

type batchStackSettingsRequest struct {
	// Match selects stacks whose name CONTAINS this substring — the same
	// semantics as the CLI `--match` / `apply --skip`. Required + non-empty: an
	// empty match would hit every stack, too blunt to do by accident.
	Match string `json:"match"`
	// MintHostname: nil → 400; false → headless; true → re-enable. Applied to
	// every matched stack (same per-stack semantics as patchStackSettingsRequest).
	MintHostname *bool `json:"mint_hostname,omitempty"`
	// Force authorizes revoking live routing hosts when going headless. Without
	// it, a batch that would tear down ANY live URL is refused (409) so a bulk
	// --no-host can't silently break links.
	Force bool `json:"force,omitempty"`
}

type batchStackSettingsResponse struct {
	Matched      int      `json:"matched"`       // stacks whose mint_hostname was set
	MintHostname bool     `json:"mint_hostname"` // the value applied
	RevokedHosts []string `json:"revoked_hosts,omitempty"`
}

// handleBatchStackSettings: POST /v1/tenants/{t}/stacks/settings
//
// The bulk sibling of handlePatchStackSettings: flips mint_hostname on EVERY
// stack whose name contains `match`, in a SINGLE transaction with a SINGLE
// dbcache reload — so headless-ing a large family (e.g. all `publications/*`)
// costs one tx + one reload instead of one per stack (1400 reloads would hammer
// the runtime DB). Going headless with `force` revokes each matched stack's live
// routing hosts the same fleet-safe way the per-stack path does (queued
// hostname.revoked events, applied across the fleet). Without force, a batch
// that would revoke ANY live host is refused (409) naming a sample, so a bulk
// --no-host can't quietly tear down URLs.
func (c *Controller) handleBatchStackSettings(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	var req batchStackSettingsRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
			return
		}
	}
	if req.MintHostname == nil {
		writeJSONError(w, http.StatusBadRequest, "no_settings", map[string]any{"reason": "no settings provided (expected mint_hostname)"})
		return
	}
	if strings.TrimSpace(req.Match) == "" {
		writeJSONError(w, http.StatusBadRequest, "empty_match", map[string]any{"reason": "match is required (an empty match would hit every stack)"})
		return
	}
	goingHeadless := !*req.MintHostname

	// Resolve matched stacks by substring on name — read the tenant's set and
	// filter in Go so the match is byte-identical to the CLI `--match`/`--skip`
	// semantics (and sidesteps SQL LIKE-escaping of the user's substring).
	type matchedStack struct{ id, name string }
	rows, err := c.pu.RuntimeDB.QueryContext(r.Context(),
		`SELECT stack_id, name FROM stacks WHERE tenant_id = ? ORDER BY name`, ac.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_stacks", map[string]any{"err": err.Error()})
		return
	}
	var matched []matchedStack
	for rows.Next() {
		var ms matchedStack
		if err := rows.Scan(&ms.id, &ms.name); err != nil {
			rows.Close()
			writeJSONError(w, http.StatusInternalServerError, "scan_stacks", map[string]any{"err": err.Error()})
			return
		}
		if strings.Contains(ms.name, req.Match) {
			matched = append(matched, ms)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeJSONError(w, http.StatusInternalServerError, "list_stacks", map[string]any{"err": err.Error()})
		return
	}
	rows.Close()
	if len(matched) == 0 {
		writeJSONError(w, http.StatusNotFound, "no_match", map[string]any{"match": req.Match})
		return
	}

	// Going headless on stacks with live URLs is destructive — gather the hosts
	// BEFORE the tx so the force-check is a clean precondition, and refuse the
	// WHOLE batch without --force (don't half-apply).
	hostsByStack := map[string][]tenants.Hostname{}
	var withLive []string
	if goingHeadless {
		for _, ms := range matched {
			hosts, herr := c.listManagedStackHosts(r.Context(), ac.TenantID, ms.name)
			if herr != nil {
				writeJSONError(w, http.StatusInternalServerError, "lookup_hosts", map[string]any{"err": herr.Error(), "stack": ms.name})
				return
			}
			if len(hosts) > 0 {
				hostsByStack[ms.name] = hosts
				withLive = append(withLive, ms.name)
			}
		}
		if len(withLive) > 0 && !req.Force {
			sample := withLive
			if len(sample) > 10 {
				sample = sample[:10]
			}
			writeJSONError(w, http.StatusConflict, "live_url_exists", map[string]any{
				"count":  len(withLive),
				"stacks": sample,
				"hint":   "matched stacks have live URLs; re-run with --force to revoke them and go headless",
			})
			return
		}
		// Revoking is a distinct capability — require it on the forced path.
		if len(withLive) > 0 {
			if err := policy.RequireCapability(r.Context(), "hostname:*:write"); err != nil {
				auth.WriteForbidden(w, signature.ErrCapabilityDenied)
				return
			}
		}
	}

	nowT := time.Now().UTC()
	mint := 0
	if *req.MintHostname {
		mint = 1
	}

	// One tx for the whole batch: atomic, and a single write-lock hold + single
	// reload instead of one per stack. (If a tenant ever has a match set large
	// enough that this lock-hold matters, chunking is the follow-up — busy_timeout
	// is 30s and these are sub-second row updates, so one tx is fine in practice.)
	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, ms := range matched {
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE stacks SET mint_hostname = ? WHERE tenant_id = ? AND name = ?`,
			mint, ac.TenantID, ms.name); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "update_settings", map[string]any{"err": err.Error(), "stack": ms.name})
			return
		}
	}
	revoked := make([]string, 0)
	if goingHeadless && req.Force {
		for _, ms := range matched {
			for _, h := range hostsByStack[ms.name] {
				if err := c.revokeManagedHostTx(r.Context(), tx, ac.TenantID, h, nowT); err != nil {
					writeJSONError(w, http.StatusInternalServerError, "revoke_host", map[string]any{"err": err.Error(), "hostname": h.Hostname})
					return
				}
				revoked = append(revoked, h.Hostname)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	// A revoke changes data-plane routing (the resolver reads tenant_hostnames),
	// so refresh the cache ONCE for the whole batch. The column flip alone needs
	// no reload (mint_hostname is read only at activate).
	if len(revoked) > 0 {
		if err := c.pu.Dbc.Reload(); err != nil {
			c.pu.Logger.Warn("dbcache reload after batch stack-headless revoke failed; FS watcher will retry",
				zap.String("err", err.Error()))
		}
	}

	c.pu.Logger.Info("batch stack settings applied",
		zap.String("tenant", ac.TenantID),
		zap.String("match", req.Match),
		zap.Int("matched", len(matched)),
		zap.Bool("mint", mint != 0),
		zap.Int("revoked", len(revoked)),
		zap.String("user", authedUser(r)))

	writeJSON(w, http.StatusOK, batchStackSettingsResponse{
		Matched:      len(matched),
		MintHostname: mint != 0,
		RevokedHosts: revoked,
	})
}

// listManagedStackHosts returns the live chassis-minted routing hosts bound to
// (tenant, stack) — the structured-host-suffix and delegated-zone rows that the
// activate path auto-mints. Custom/user-bound hostnames (other created_by) are
// intentionally excluded: `txco stack set --no-host` only owns what the chassis
// minted. Usually 0 or 1 row, so the per-host LookupActiveHostname (to get the
// full row incl. DKIM for the fleet artifact) is fine.
func (c *Controller) listManagedStackHosts(ctx context.Context, tenantID, stack string) ([]tenants.Hostname, error) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		`SELECT hostname FROM tenant_hostnames
		  WHERE tenant_id = ? AND stack = ? AND revoked_at IS NULL
		    AND created_by IN (?, ?)
		  ORDER BY hostname`,
		tenantID, stack, tenants.SystemStructuredHostCreatedBy, tenants.SystemZoneHostCreatedBy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]tenants.Hostname, 0, len(names))
	for _, n := range names {
		h, err := c.tenants.LookupActiveHostname(ctx, n)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// revokeManagedHostTx soft-deletes one routing host the fleet-safe way, mirroring
// handleRevokeHostname: upload the revoked-row artifact (so data-plane nodes
// learn the soft-delete) BEFORE appending the outbox event, then RevokeHostnameTx
// + queue a hostname.revoked event, all inside the caller's tx.
func (c *Controller) revokeManagedHostTx(ctx context.Context, tx *sql.Tx, tenantID string, h tenants.Hostname, now time.Time) error {
	var ref, sum string
	if c.fleetEnabled() {
		postRevoke := h
		postRevoke.RevokedAt = &now
		r, s, err := c.fleetUploadHostnameUpsert(ctx, postRevoke)
		if err != nil {
			return fmt.Errorf("fleet upload: %w", err)
		}
		ref, sum = r, s
	}
	if _, _, err := c.tenants.RevokeHostnameTx(ctx, tx, h.Hostname); err != nil {
		return fmt.Errorf("revoke: %w", err)
	}
	if c.fleetEnabled() {
		if _, err := c.fleetQueueEvent(ctx, tx,
			controlevent.TypeHostnameRevoked, tenantID, "", 0, 0, ref, sum); err != nil {
			return fmt.Errorf("queue revoke event: %w", err)
		}
	}
	return nil
}

// handleListVersions: GET /v1/tenants/{t}/stacks/{name}/versions
func (c *Controller) handleListVersions(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	name := mux.Vars(r)["name"]
	stackID, activeVersionID, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	rows, err := c.pu.RuntimeDB.QueryContext(r.Context(),
		`SELECT v.version_id, v.version_number, v.status, v.created_by, v.created_at,
		        v.activated_at, v.manifest_hash, p.version_number AS parent_n
		   FROM stack_versions v
		   LEFT JOIN stack_versions p ON p.version_id = v.parent_version_id
		  WHERE v.stack_id = ?
		  ORDER BY v.version_number DESC`, stackID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	defer rows.Close()
	var out []versionRecord
	for rows.Next() {
		var v versionRecord
		var vID int64
		var activatedAt sql.NullString
		var parentN sql.NullInt64
		if err := rows.Scan(&vID, &v.VersionNumber, &v.Status, &v.CreatedBy, &v.CreatedAt,
			&activatedAt, &v.ManifestHash, &parentN); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "scan_failed", map[string]any{"err": err.Error()})
			return
		}
		if activatedAt.Valid {
			v.ActivatedAt = &activatedAt.String
		}
		if parentN.Valid {
			v.ParentVersion = &parentN.Int64
		}
		v.IsActive = activeVersionID.Valid && activeVersionID.Int64 == vID
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, listVersionsResponse{Versions: out})
}

// handleGetVersion: GET /v1/tenants/{t}/stacks/{name}/versions/{n}
// Query: ?include=content inlines every file body (txco pull / editor);
// ?include=ops inlines only op/mock bodies and skips FILES/ assets (the ops
// view, which never renders assets — avoids a full-book R2 fan-out per stack).
func (c *Controller) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	vars := mux.Vars(r)
	name := vars["name"]
	n, err := strconv.ParseInt(vars["n"], 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_version_number", nil)
		return
	}

	stackID, activeVersionID, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}

	var v versionRecord
	var vID int64
	var activatedAt sql.NullString
	var parentN sql.NullInt64
	err = c.pu.RuntimeDB.QueryRowContext(r.Context(),
		`SELECT v.version_id, v.version_number, v.status, v.created_by, v.created_at,
		        v.activated_at, v.manifest_hash, p.version_number
		   FROM stack_versions v
		   LEFT JOIN stack_versions p ON p.version_id = v.parent_version_id
		  WHERE v.stack_id = ? AND v.version_number = ?`, stackID, n).
		Scan(&vID, &v.VersionNumber, &v.Status, &v.CreatedBy, &v.CreatedAt,
			&activatedAt, &v.ManifestHash, &parentN)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "version_not_found", map[string]any{"version_number": n})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	if activatedAt.Valid {
		v.ActivatedAt = &activatedAt.String
	}
	if parentN.Valid {
		v.ParentVersion = &parentN.Int64
	}
	v.IsActive = activeVersionID.Valid && activeVersionID.Int64 == vID

	mode := contentNone
	switch r.URL.Query().Get("include") {
	case "content":
		mode = contentAll
	case "ops":
		mode = contentOps
	}
	files, err := c.loadVersionFiles(r.Context(), vID, mode)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "file_load_failed", map[string]any{"err": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, versionDetail{versionRecord: v, Files: files})
}

// handleCatFile resolves a single FILES/ asset of a stack's ACTIVE version the
// same way txco://read-file does — manifest row → inline content, else the
// content-addressed store keyed by content_hash — and reports exactly where the
// resolution succeeds or fails (manifest miss / empty hash / CAS miss). A
// debugging + ops probe: `txco cat <stack> <path>`.
func (c *Controller) handleCatFile(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	name := mux.Vars(r)["name"]
	rel := r.URL.Query().Get("path")
	if rel == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_path", nil)
		return
	}
	// Accept either the read-file-style path ("_data/x") or a full "FILES/_data/x".
	stored := rel
	if !strings.HasPrefix(stored, "FILES/") {
		stored = "FILES/" + strings.TrimPrefix(rel, "/")
	}

	_, activeVersionID, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	resp := map[string]any{"stack": name, "path": stored}
	if !activeVersionID.Valid {
		resp["found"] = false
		resp["reason"] = "stack has no active version"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp["active_version_id"] = activeVersionID.Int64

	var content, hash string
	err = c.pu.RuntimeDB.QueryRowContext(r.Context(),
		`SELECT content, content_hash FROM stack_files WHERE version_id = ? AND path = ?`,
		activeVersionID.Int64, stored).Scan(&content, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		resp["found"] = false
		resp["reason"] = "no stack_files row for (active version, path) — not in the deployed manifest"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "query_failed", map[string]any{"err": err.Error()})
		return
	}
	resp["content_hash"] = hash
	resp["inline_len"] = len(content)

	switch {
	case content != "":
		resp["found"] = true
		resp["source"] = "inline"
		resp["content_b64"] = base64.StdEncoding.EncodeToString([]byte(content))
	case hash == "":
		resp["found"] = false
		resp["reason"] = "row present but content AND content_hash are empty — read-file's index skips this"
	case c.fcas == nil:
		resp["found"] = false
		resp["reason"] = "fingerprint-only row but this node has no content-addressed store"
	default:
		b, gerr := c.fcas.Get(r.Context(), hash)
		if gerr != nil {
			resp["found"] = false
			resp["source"] = "cas"
			resp["reason"] = "manifest has hash " + hash + " but the CAS has no bytes: " + gerr.Error()
		} else {
			resp["found"] = true
			resp["source"] = "cas"
			resp["size"] = len(b)
			resp["content_b64"] = base64.StdEncoding.EncodeToString(b)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// contentMode selects which file bodies loadVersionFiles resolves. Asset bytes
// live in the content-addressed store (R2 in prod), so resolving them is a
// network fetch per file. The ops view renders only txcl + mock JSON and ignores
// every FILES/ asset, so it must NOT force a full-book download per stack — that
// fan-out (one GET per asset × every stack) is what saturates the connection pool
// and turns client-cancelled requests into file_load_failed 500s.
//
//	contentNone — list paths + hashes only (no CAS reads)
//	contentOps  — resolve op/mock bodies (non-FILES/ paths); skip FILES/ assets
//	contentAll  — resolve every file body (txco pull / editor / validate)
type contentMode int

const (
	contentNone contentMode = iota
	contentOps
	contentAll
)

// wantsBytes reports whether path's body should be resolved under this mode.
// Op/mock sources are stored without a FILES/ prefix; runtime assets carry it.
func (m contentMode) wantsBytes(path string) bool {
	switch m {
	case contentAll:
		return true
	case contentOps:
		return !strings.HasPrefix(path, "FILES/")
	default:
		return false
	}
}

func (c *Controller) loadVersionFiles(ctx context.Context, versionID int64, mode contentMode) ([]stackFile, error) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		`SELECT path, content, content_hash FROM stack_files
		  WHERE version_id = ? ORDER BY path`, versionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	emptyHash := sha256Hex("")
	var out []stackFile
	for rows.Next() {
		var f stackFile
		var content string
		if err := rows.Scan(&f.Path, &content, &f.ContentHash); err != nil {
			return nil, err
		}
		if f.ContentHash == "" {
			// Backfilled rows have empty hashes; compute lazily.
			f.ContentHash = sha256Hex(content)
		}
		if mode.wantsBytes(f.Path) {
			// A materialised version strips the content column — the bytes live in the
			// filecas (a 0-byte stub here is why `txco pull` wrote empty files). Resolve
			// them from the CAS, like handleCatFile. An empty hash is a genuinely-empty
			// file, not a stripped one → leave it empty, don't touch the CAS.
			if content == "" && f.ContentHash != "" && f.ContentHash != emptyHash {
				if c.fcas == nil {
					return nil, fmt.Errorf("loadVersionFiles %s: fingerprint-only but no filecas on this node", f.Path)
				}
				b, gerr := c.fcas.Get(ctx, f.ContentHash)
				if gerr != nil {
					return nil, fmt.Errorf("loadVersionFiles %s (cas %s): %w", f.Path, f.ContentHash, gerr)
				}
				content = string(b)
			}
			// Non-UTF-8 (binary) bytes go base64 so json.Marshal can't rewrite them to
			// U+FFFD; the client decodes before writing to disk.
			if utf8.Valid([]byte(content)) {
				f.Content = content
			} else {
				f.Content = base64.StdEncoding.EncodeToString([]byte(content))
				f.Encoding = "base64"
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// handleCreateDraft: POST /v1/tenants/{t}/stacks/{name}/draft
// Creates a stack if it doesn't exist (auto-vivify on first push).
// Returns the new version_number.
func (c *Controller) handleCreateDraft(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	name := mux.Vars(r)["name"]
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "empty_stack_name", nil)
		return
	}
	if err := validateStackName(name, ac.TenantID); err != nil {
		writeJSONError(w, http.StatusBadRequest, "reserved_stack_name", map[string]any{"reason": err.Error()})
		return
	}
	var req createDraftRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
			return
		}
	}

	user := authedUser(r)
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Look up or auto-vivify the stack.
	var stackID string
	var activeVersionID sql.NullInt64
	row := tx.QueryRowContext(r.Context(),
		`SELECT stack_id, active_version FROM stacks WHERE tenant_id = ? AND name = ?`,
		ac.TenantID, name)
	err = row.Scan(&stackID, &activeVersionID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		stackID = "stk_" + hxid.NewTimeSort().String()
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES (?, ?, ?, ?)`,
			stackID, ac.TenantID, name, now); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "create_stack", map[string]any{"err": err.Error()})
			return
		}
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_stack", map[string]any{"err": err.Error()})
		return
	}

	// Determine source version_id to clone from (if any).
	var sourceVersionID sql.NullInt64
	switch {
	case req.From == "" || req.From == "active":
		sourceVersionID = activeVersionID
	default:
		n, err := strconv.ParseInt(req.From, 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_from", map[string]any{"from": req.From})
			return
		}
		var vID int64
		if err := tx.QueryRowContext(r.Context(),
			`SELECT version_id FROM stack_versions WHERE stack_id = ? AND version_number = ?`,
			stackID, n).Scan(&vID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeJSONError(w, http.StatusBadRequest, "from_version_not_found", map[string]any{"version_number": n})
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "lookup_from", map[string]any{"err": err.Error()})
			return
		}
		sourceVersionID = sql.NullInt64{Int64: vID, Valid: true}
	}

	// Assign next version_number for this stack.
	var nextN int64
	if err := tx.QueryRowContext(r.Context(),
		`SELECT COALESCE(MAX(version_number), 0) + 1 FROM stack_versions WHERE stack_id = ?`,
		stackID).Scan(&nextN); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "next_version_number", map[string]any{"err": err.Error()})
		return
	}

	res, err := tx.ExecContext(r.Context(),
		`INSERT INTO stack_versions
		    (stack_id, version_number, parent_version_id, status, created_by, created_at, manifest_hash)
		 VALUES (?, ?, ?, 'draft', ?, ?, '')`,
		stackID, nextN, nullableInt(sourceVersionID), user, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create_version", map[string]any{"err": err.Error()})
		return
	}
	draftVersionID, _ := res.LastInsertId()

	// Clone files from source if any.
	if sourceVersionID.Valid {
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO stack_files (version_id, path, content, content_hash)
			 SELECT ?, path, content, content_hash FROM stack_files WHERE version_id = ?`,
			draftVersionID, sourceVersionID.Int64); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "clone_files", map[string]any{"err": err.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	writeJSON(w, http.StatusOK, createDraftResponse{VersionNumber: nextN})
}

func nullableInt(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// handlePutDraftFiles: PUT /v1/tenants/{t}/stacks/{name}/versions/{n}/files
// Replaces the entire file set of a draft version. Body: {files: [{path, content}]}.
// 409 if version is not status='draft'.
func (c *Controller) handlePutDraftFiles(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	vars := mux.Vars(r)
	name := vars["name"]
	n, err := strconv.ParseInt(vars["n"], 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_version_number", nil)
		return
	}

	var req putFilesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
		return
	}
	// Compute content hashes, dedupe paths, validate each path. The
	// validator applies the same rules as PATCH/DELETE so a draft
	// committed via PUT never contains a path those endpoints would
	// later reject.
	seen := map[string]bool{}
	for i := range req.Files {
		f := &req.Files[i]
		if err := validateStackFilePath(f.Path); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_path",
				map[string]any{"index": i, "path": f.Path, "reason": err.Error()})
			return
		}
		if seen[f.Path] {
			writeJSONError(w, http.StatusBadRequest, "duplicate_path", map[string]any{"path": f.Path})
			return
		}
		seen[f.Path] = true
		// Binary assets arrive base64-encoded (the wire is JSON/UTF-8); decode to raw
		// bytes BEFORE hashing + storing, so the stored content and its hash (the
		// content-addressed store key) are the real file bytes.
		if f.Encoding == "base64" {
			raw, derr := base64.StdEncoding.DecodeString(f.Content)
			if derr != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_base64",
					map[string]any{"index": i, "path": f.Path, "err": derr.Error()})
				return
			}
			f.Content = string(raw)
			f.Encoding = ""
		}
		f.ContentHash = sha256Hex(f.Content)
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	stackID, _, err := c.lookupStack(r.Context(), tx, ac.TenantID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_stack", map[string]any{"err": err.Error()})
		return
	}

	versionID, status, err := c.lookupVersion(r.Context(), tx, stackID, n)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "version_not_found", map[string]any{"version_number": n})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_version", map[string]any{"err": err.Error()})
		return
	}
	if status != "draft" {
		writeJSONError(w, http.StatusConflict, "version_not_draft", map[string]any{"status": status})
		return
	}

	// Clear the managed category's existing files and re-insert. The draft was
	// cloned from the active version, so the UNmanaged category's rows survive
	// (carried forward): "code" preserves the store-seed packs, "data" preserves
	// the code, "" / "all" replaces everything. This is what makes data opt-in —
	// a `txco apply` (code) never strips a stack's packs.
	delQuery := `DELETE FROM stack_files WHERE version_id = ?`
	switch req.Manage {
	case "code":
		delQuery = `DELETE FROM stack_files WHERE version_id = ?
			   AND NOT (path LIKE 'VECTORS/%' OR path LIKE 'KV/%')`
	case "data":
		delQuery = `DELETE FROM stack_files WHERE version_id = ?
			   AND (path LIKE 'VECTORS/%' OR path LIKE 'KV/%')`
	}
	if _, err := tx.ExecContext(r.Context(), delQuery, versionID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "clear_files", map[string]any{"err": err.Error()})
		return
	}
	for _, f := range req.Files {
		// content_hash is BOTH the content-addressed store key (materialise)
		// AND the static-index fingerprint (RebuildTenant skips empty-hash
		// rows). The client may omit it — collectFileAssets historically sent
		// "" for FILES assets — so backfill it from the content here. Without
		// this, a stack's FILES are invisible to txco://read-file. A
		// fingerprint-only row (content already stripped, hash supplied) keeps
		// its supplied hash.
		hash := f.ContentHash
		if hash == "" && f.Content != "" {
			hash = sha256Hex(f.Content)
		}
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO stack_files (version_id, path, content, content_hash)
			 VALUES (?, ?, ?, ?)`, versionID, f.Path, f.Content, hash); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "insert_file",
				map[string]any{"path": f.Path, "err": err.Error()})
			return
		}
	}

	mhash := computeManifestHash(req.Files)
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE stack_versions SET manifest_hash = ? WHERE version_id = ?`,
		mhash, versionID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "update_manifest_hash", map[string]any{"err": err.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	writeJSON(w, http.StatusOK, putFilesResponse{ManifestHash: mhash})
}

// --- activate --------------------------------------------------------

// pathFile is the parsed shape of a stack_file path: <scope>/<base>.<ext>.
// base is "<name>.txcl" for rule bodies (name=” for legacy "_legacy_N"
// filenames), or "mock-request.json"/"mock-response.json" for mock files.
type pathFile struct {
	scope     int
	name      string // empty for legacy + mock files
	isMockReq bool
	isMockRes bool
}

var pathRe = regexp.MustCompile(`^(\d+)/(.+)$`)

// parseStackPath maps a stack_files.path back to its (scope, name)
// shape so activation can materialise rows into the `ops` table.
// Unrecognised paths are silently skipped (with a logged warning) —
// they'll come back when the version is re-pushed.
func parseStackPath(p string) (pathFile, bool) {
	m := pathRe.FindStringSubmatch(p)
	if m == nil {
		return pathFile{}, false
	}
	scope, _ := strconv.Atoi(m[1])
	rest := m[2]
	switch rest {
	case "mock-request.json":
		return pathFile{scope: scope, isMockReq: true}, true
	case "mock-response.json":
		return pathFile{scope: scope, isMockRes: true}, true
	}
	if !strings.HasSuffix(rest, ".txcl") {
		return pathFile{}, false
	}
	base := strings.TrimSuffix(rest, ".txcl")
	if strings.HasPrefix(base, "_legacy_") {
		return pathFile{scope: scope}, true // legacy: name=''
	}
	return pathFile{scope: scope, name: base}, true
}

// handleActivateStack: POST /v1/tenants/{t}/stacks/{name}/activate
// Body: {version_number}. Atomic: flip status + pointer + materialise
// ops rows in one transaction.
// materialiseError carries the HTTP status/code/detail of a materialisation
// failure so the activation handler's responses stay byte-for-byte
// unchanged after the extraction below.
type materialiseError struct {
	status int
	code   string
	detail map[string]any
}

func (e *materialiseError) Error() string { return e.code }

// materialiseConcurrency bounds how many FILES/* assets materialiseFiles writes
// to the content-addressed store at once. The old code wrote them one at a time;
// for a large stack on a network (R2) CAS that ran past Cloudflare's ~100s edge
// timeout (524) on activate.
const materialiseConcurrency = 32

// materialiseFiles writes the given path→content assets into the content-
// addressed store, CONCURRENTLY (bounded) and skipping content already present
// (so an unchanged file costs no write on re-deploy). The first Put error aborts
// the rest (gctx cancels) and is returned so the caller rolls back the
// activation. Callers pass only the rows to materialise (FILES/* with non-empty
// content); non-FILES rows stay inline in ops, and an empty content is a
// fingerprint-only row whose bytes already live in the shared CAS.
func materialiseFiles(ctx context.Context, fcas filecas.Store, files map[string]string) *materialiseError {
	if fcas == nil || len(files) == 0 {
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(materialiseConcurrency)
	for path, content := range files {
		h := sha256Hex(content)
		g.Go(func() (err error) {
			// A panic in a worker goroutine would otherwise crash the WHOLE
			// chassis — the HTTP handler's recover middleware can't reach another
			// goroutine — turning a recoverable backend fault into a dropped
			// connection (502). Convert it to an error so activation rolls back
			// cleanly and the cause surfaces in the response/logs.
			defer func() {
				if r := recover(); r != nil {
					err = &materialiseError{http.StatusInternalServerError, "filecas_panic", map[string]any{"path": path, "panic": fmt.Sprint(r)}}
				}
			}()
			if ok, _ := fcas.Exists(gctx, h); ok {
				return nil
			}
			if perr := fcas.Put(gctx, h, []byte(content)); perr != nil {
				return &materialiseError{http.StatusServiceUnavailable, "filecas_put", map[string]any{"path": path, "err": perr.Error()}}
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		if me, ok := err.(*materialiseError); ok {
			return me
		}
		return &materialiseError{http.StatusServiceUnavailable, "filecas_put", map[string]any{"err": err.Error()}}
	}
	return nil
}

// materialiseStackVersion is the transactional core of activation, extracted
// verbatim from handleActivateStack so the identical logic can run from a
// non-HTTP context (e.g. a control-event applier — see the fleet sync plan).
// It resolves stack+version, flips version status, clears and re-materialises
// the (tenant, stack) ops rows from stack_files, and flips
// stacks.active_version. The caller owns tx begin/commit and any dbcache
// reload. Failures are returned as *materialiseError so callers can preserve
// the original status/code/detail. Returns (priorActiveVersionID,
// targetVersionID).
//
// mintHosts gates the auto-mint of a routing hostname: true on the
// control-plane admin path (handleActivateStack), where the canonical host is
// minted once here and then fleet-published; false on the data-plane applier
// (ApplyStackVersion). The data plane must NOT re-mint — MintHandle is random,
// so every node would diverge to its own hostname and the stack's URL would
// 404 on all but the minting node. Data-plane nodes receive the canonical host
// via a separate hostname.bound control event instead.
func (c *Controller) materialiseStackVersion(ctx context.Context, tx *sql.Tx,
	tenantID, stackName string, versionNumber int64, now string, mintHosts bool,
) (sql.NullInt64, int64, error) {

	stackID, currentActiveID, err := c.lookupStack(ctx, tx, tenantID, stackName)
	if errors.Is(err, sql.ErrNoRows) {
		return currentActiveID, 0, &materialiseError{http.StatusNotFound, "stack_not_found", map[string]any{"name": stackName}}
	}
	if err != nil {
		return currentActiveID, 0, &materialiseError{http.StatusInternalServerError, "lookup_stack", map[string]any{"err": err.Error()}}
	}

	targetVersionID, targetStatus, err := c.lookupVersion(ctx, tx, stackID, versionNumber)
	if errors.Is(err, sql.ErrNoRows) {
		return currentActiveID, 0, &materialiseError{http.StatusNotFound, "version_not_found", map[string]any{"version_number": versionNumber}}
	}
	if err != nil {
		return currentActiveID, 0, &materialiseError{http.StatusInternalServerError, "lookup_version", map[string]any{"err": err.Error()}}
	}
	if targetStatus == "revoked" {
		return currentActiveID, targetVersionID, &materialiseError{http.StatusConflict, "version_revoked", nil}
	}

	// Flip status to 'superseded' if still draft; record activated_at on
	// first activation.
	if _, err := tx.ExecContext(ctx,
		`UPDATE stack_versions
		    SET status = 'superseded',
		        activated_at = COALESCE(activated_at, ?)
		  WHERE version_id = ?`, now, targetVersionID); err != nil {
		return currentActiveID, targetVersionID, &materialiseError{http.StatusInternalServerError, "update_version_status", map[string]any{"err": err.Error()}}
	}

	// Materialise into ops: clear this (tenant, stack) and re-insert from
	// stack_files. The runtime keeps reading ops directly.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ops WHERE tenant_id = ? AND stack = ?`, tenantID, stackName); err != nil {
		return currentActiveID, targetVersionID, &materialiseError{http.StatusInternalServerError, "clear_ops", map[string]any{"err": err.Error()}}
	}

	frows, err := tx.QueryContext(ctx,
		`SELECT path, content FROM stack_files WHERE version_id = ?`, targetVersionID)
	if err != nil {
		return currentActiveID, targetVersionID, &materialiseError{http.StatusInternalServerError, "load_files", map[string]any{"err": err.Error()}}
	}
	type rawFile struct {
		path    string
		content string
	}
	var rawFiles []rawFile
	for frows.Next() {
		var rf rawFile
		if err := frows.Scan(&rf.path, &rf.content); err != nil {
			_ = frows.Close()
			return currentActiveID, targetVersionID, &materialiseError{http.StatusInternalServerError, "scan_files", map[string]any{"err": err.Error()}}
		}
		rawFiles = append(rawFiles, rf)
	}
	_ = frows.Close()

	// Persist FILES/* asset bytes into the content-addressed store so the
	// static serve path resolves them lazily by hash; .txcl / mock-* stay
	// inline in ops. The store verifies sha256==hash and is create-if-absent
	// (idempotent dedup). A failure aborts activation via the caller's
	// rollback rather than half-publishing. Also fires on fleet apply
	// (applier → ApplyStackVersion → here), so nodes self-populate their CAS
	// from the still-inline content (Phase 1).
	// Materialise FILES/* bytes into the content-addressed store. On a FLEET
	// node the producer ALREADY did this BEFORE the tx (handleActivateStack →
	// readStackFilesForArtifact puts every FILES/ asset to the shared CAS), so
	// we MUST skip it here. The runtime DB is single-connection
	// (dbcache.MaxOpenConns=1); doing the per-file R2 round-trips while the
	// activation tx pins that connection stalls the WHOLE node — healthz and
	// inbound mail time out (the `context canceled` storm), and a starved
	// dbcache reload can leave the static index stale. Single-node deployments
	// have no pre-tx producer, so they still materialise here, against their
	// (local, fast) CAS where holding the connection briefly is harmless.
	if c.fcas != nil && !c.fleetEnabled() {
		assets := make(map[string]string)
		for _, rf := range rawFiles {
			if strings.HasPrefix(rf.path, "FILES/") && rf.content != "" {
				assets[rf.path] = rf.content
			}
		}
		if me := materialiseFiles(ctx, c.fcas, assets); me != nil {
			return currentActiveID, targetVersionID, me
		}
	}

	// Group by scope: collect rules + per-scope mocks, then emit one ops
	// row per (scope, name) with the scope's mocks attached.
	type scopeData struct {
		rules   map[string]string
		mockReq string
		mockRes string
	}
	scopes := map[int]*scopeData{}
	getScope := func(s int) *scopeData {
		if sd, ok := scopes[s]; ok {
			return sd
		}
		sd := &scopeData{rules: map[string]string{}}
		scopes[s] = sd
		return sd
	}
	for _, rf := range rawFiles {
		if strings.HasPrefix(rf.path, "FILES/") {
			continue // static asset → filecas (above), not an ops row
		}
		if storeseed.IsPackPath(rf.path) {
			continue // store-seed pack → reconciled post-commit, not an ops row
		}
		pf, ok := parseStackPath(rf.path)
		if !ok {
			c.pu.Logger.Warn("activate: skipping unrecognised file path",
				zap.String("stack", stackName), zap.String("path", rf.path))
			continue
		}
		sd := getScope(pf.scope)
		switch {
		case pf.isMockReq:
			sd.mockReq = rf.content
		case pf.isMockRes:
			sd.mockRes = rf.content
		default:
			// Guardrail: an unresolved op:// ref must fail loudly here
			// rather than materialise into ops and fail silently at
			// runtime. The caller's deferred rollback aborts activation.
			if oprefs.HasRefs(rf.content) {
				return currentActiveID, targetVersionID, &materialiseError{http.StatusUnprocessableEntity, "unresolved_op_ref", map[string]any{
					"stack": stackName,
					"scope": pf.scope,
					"name":  pf.name,
					"ops":   oprefs.References(rf.content),
					"hint":  "define it under operations: in txco.yaml and run `txco apply`, or use an explicit http(s):// URL",
				}}
			}
			// Guardrail: every compute://<alg>/<digest> a rule references
			// must already be in the artifact store, else the rule would
			// materialise and fail at runtime with "artifact not found".
			// Fail loudly here; the deferred rollback aborts activation.
			if refs := compute.ScanRefs(rf.content); len(refs) > 0 {
				if c.astore == nil {
					return currentActiveID, targetVersionID, &materialiseError{http.StatusServiceUnavailable, "compute_store_unavailable", map[string]any{
						"stack": stackName, "scope": pf.scope, "name": pf.name,
						"hint": "this chassis has no artifact store; compute:// rules cannot be activated here",
					}}
				}
				for _, ref := range refs {
					ok, err := c.astore.Exists(ctx, ref.StoreRef())
					if err != nil {
						return currentActiveID, targetVersionID, &materialiseError{http.StatusInternalServerError, "compute_store_check", map[string]any{"ref": ref.String(), "err": err.Error()}}
					}
					if !ok {
						return currentActiveID, targetVersionID, &materialiseError{http.StatusUnprocessableEntity, "missing_compute_artifact", map[string]any{
							"stack": stackName, "scope": pf.scope, "name": pf.name,
							"ref":  ref.String(),
							"hint": "upload the compute artifact (txco compute build + apply) before activating",
						}}
					}
				}
			}
			sd.rules[pf.name] = rf.content
		}
	}

	for scope, sd := range scopes {
		for nm, txc := range sd.rules {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				tenantID, stackName, scope, nm, txc, sd.mockReq, sd.mockRes); err != nil {
				return currentActiveID, targetVersionID, &materialiseError{http.StatusInternalServerError, "insert_ops", map[string]any{"scope": scope, "name": nm, "err": err.Error()}}
			}
		}
	}

	// Flip the pointer.
	if _, err := tx.ExecContext(ctx,
		`UPDATE stacks SET active_version = ? WHERE stack_id = ?`,
		targetVersionID, stackID); err != nil {
		return currentActiveID, targetVersionID, &materialiseError{http.StatusInternalServerError, "update_active_version", map[string]any{"err": err.Error()}}
	}

	// Auto-mint a routing hostname so the stack is reachable with no
	// manual binding. If the tenant has delegated a DNS zone to us, the
	// stack is wired under that zone (`stack-name.<origin>`, resolved by
	// our dns head — internal docs/todo-dns-authority.md); otherwise it
	// falls back to the global structured-host suffix
	// (internal docs/todo-structured-stack-hostnames.md). It rides this
	// same tx so the row is atomic with the version flip. Skips system
	// stacks. A mint failure must NEVER fail an activation — log and
	// continue; the convenience hostname is secondary to the deploy.
	//
	// mintHosts is the control-plane gate: only the admin handler mints
	// (then fleet-publishes the row); the data-plane applier passes false
	// so it never mints a divergent random host (see the func doc).
	if mintHosts && isMintableStack(stackName) && c.stackMintsHost(ctx, tx, tenantID, stackName) {
		origin, ok, zerr := tenants.ActivePatternZoneOriginTx(ctx, tx, tenantID)
		if zerr != nil {
			// A zone-lookup failure must never skip the structured-host
			// fallback (or fail activation): log and treat it as "no
			// delegated zone", preserving the pre-delegated-zone behavior.
			c.pu.Logger.Warn("delegated-zone lookup failed; using structured-host suffix",
				zap.String("tenant", tenantID), zap.String("stack", stackName),
				zap.String("err", zerr.Error()))
			ok = false
		}
		switch {
		case ok:
			if _, merr := tenants.EnsureZoneHostnameTx(ctx, tx, tenantID, stackName, origin, now); merr != nil {
				c.pu.Logger.Warn("zone hostname mint skipped (activation unaffected)",
					zap.String("tenant", tenantID), zap.String("stack", stackName),
					zap.String("origin", origin), zap.String("err", merr.Error()))
			}
		case c.pu.Conf.StructuredHostSuffix != "":
			if _, merr := tenants.EnsureSystemHostnameTx(ctx, tx, tenantID, stackName, c.pu.Conf.StructuredHostSuffix, now); merr != nil {
				c.pu.Logger.Warn("structured hostname mint skipped (activation unaffected)",
					zap.String("tenant", tenantID), zap.String("stack", stackName),
					zap.String("err", merr.Error()))
			}
		}
	}

	return currentActiveID, targetVersionID, nil
}

// isMintableStack reports whether a stack should get an auto-minted
// structured (web) hostname. Any stack with a `_`-prefixed segment never
// does — chassis system stacks (`_sys`, the continuation stack) AND
// per-tenant convention handlers, whether at the root (`_cron`, `_mail`)
// or NESTED under an app stack (`test-01/_mail`, `test-01/_cron`). Those
// are mail/cron/system machinery, not web apps, so a web hostname for
// them is meaningless (and confusing). The boot stack is excluded too.
func isMintableStack(stack string) bool {
	// HasPrefix catches a `_`-prefixed root segment; Contains("/_")
	// catches a `_`-prefixed nested segment (e.g. `test-01/_mail`).
	if stack == "" || strings.HasPrefix(stack, "_") || strings.Contains(stack, "/_") {
		return false
	}
	ls := strings.ToLower(stack)
	if ls == "boot" || strings.HasPrefix(ls, "boot/") || ls == "txc-continuation" {
		return false
	}
	return true
}

// structuredURL builds the reachable URL for a minted hostname. Scheme
// is derived from the request (mirrors browserLoginURL/isSecureRequest):
// https when TLS-terminated (Caddy fronts 443 — no port), plain http
// otherwise with the configured web port appended (dev →
// http://<host>.localhost:8080). The host is the minted structured
// hostname, NOT r.Host (that's the admin host).
func structuredURL(r *http.Request, host, webAddr string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = p
	}
	if scheme == "https" {
		return "https://" + host
	}
	port := webAddr
	if i := strings.LastIndex(webAddr, ":"); i >= 0 {
		port = webAddr[i+1:]
	}
	if port != "" && port != "80" {
		return "http://" + host + ":" + port
	}
	return "http://" + host
}

// ApplyStackVersion is the non-HTTP entry point to the activation core,
// used by the control-event applier (chassis/controlapply) to materialise a
// stack.activated event. It runs the same transactional logic the admin
// handler uses; the caller owns tx begin/commit and any dbcache reload. The
// HTTP-only status/code/detail is collapsed to a plain error.
func (c *Controller) ApplyStackVersion(ctx context.Context, tx *sql.Tx,
	tenantID, stack string, version int64, now string) error {
	// mintHosts=false: the data-plane applier must not mint its own routing
	// hostname (MintHandle is random → divergent per node). The canonical
	// host is minted once on the control plane and arrives as a separate
	// hostname.bound event.
	if _, _, err := c.materialiseStackVersion(ctx, tx, tenantID, stack, version, now, false); err != nil {
		return err
	}
	return nil
}

func (c *Controller) handleActivateStack(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:activate"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	name := mux.Vars(r)["name"]
	if err := validateStackName(name, ac.TenantID); err != nil {
		// Defence in depth: a draft created before this guard existed
		// must not be allowed to materialise into the ops table.
		writeJSONError(w, http.StatusBadRequest, "reserved_stack_name", map[string]any{"reason": err.Error()})
		return
	}

	var req activateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
		return
	}
	if req.VersionNumber <= 0 {
		writeJSONError(w, http.StatusBadRequest, "missing_version_number", nil)
		return
	}

	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	// Fleet-sync producer: read the target version's files BEFORE
	// opening the tx so the artifact upload runs out of the lock.
	// Stack versions are immutable (per the contract) so this is
	// race-free for file contents. Upload happens before the tx —
	// orphan artifacts (no committed outbox row) are tolerated and
	// GC'd by the sweeper; an accepted DB mutation without its
	// artifact would be unrecoverable, hence the ordering.
	var (
		fleetArtifactRef string
		fleetChecksum    string
		fleetBaseVersion int64
	)
	if c.fleetEnabled() {
		files, ferr := c.readStackFilesForArtifact(r.Context(), ac.TenantID, name, req.VersionNumber)
		if ferr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_read_files",
				map[string]any{"err": ferr.Error()})
			return
		}
		// Producer's view of the prior active_version, recorded as
		// base_version for future CAS-style concurrency checks. Best-
		// effort: if active_version is unset (first activation) we
		// record 0.
		fleetBaseVersion = c.currentActiveVersionNumber(r.Context(), ac.TenantID, name)
		art := controlevent.StackActivatedArtifact{
			TenantID: ac.TenantID,
			Stack:    name,
			Version:  req.VersionNumber,
			Files:    files,
		}
		key := fmt.Sprintf("stacks/%s/%s/%d", ac.TenantID, name, req.VersionNumber)
		ref, sum, _, uerr := c.fleetUploadArtifact(r.Context(), key, art)
		if uerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_upload",
				map[string]any{"err": uerr.Error()})
			return
		}
		fleetArtifactRef = ref
		fleetChecksum = sum
	}

	// BEGIN IMMEDIATE so SQLite takes a RESERVED lock up front; concurrent
	// activations on the same chassis serialise rather than racing into
	// a half-applied state.
	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	if _, err := tx.ExecContext(r.Context(), "BEGIN IMMEDIATE"); err != nil {
		// SQLite's database/sql driver opens its own implicit tx; the
		// explicit BEGIN IMMEDIATE inside it fails with "cannot start a
		// transaction within a transaction". Fall through — the outer
		// BeginTx already gives us isolation against same-connection
		// concurrent writers, which is all `dbcache.MaxOpenConns=1`
		// allows in practice.
		_ = err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	currentActiveID, targetVersionID, merr := c.materialiseStackVersion(
		r.Context(), tx, ac.TenantID, name, req.VersionNumber, now, true)
	if merr != nil {
		var me *materialiseError
		if errors.As(merr, &me) {
			writeJSONError(w, me.status, me.code, me.detail)
		} else {
			writeJSONError(w, http.StatusInternalServerError, "activate_failed",
				map[string]any{"err": merr.Error()})
		}
		return
	}

	// Fleet-sync producer: queue the outbox row inside the same tx
	// as the activation. The pump (chassis/controlpublish) drains it
	// asynchronously; the broker assigns control_version on publish.
	// Skipped entirely when --feed-sink=nop.
	if c.fleetEnabled() {
		// Look up stack_id inside the tx — guaranteed to exist after
		// materialiseStackVersion since that path upserts it.
		var stackID string
		if err := tx.QueryRowContext(r.Context(),
			`SELECT stack_id FROM stacks WHERE tenant_id = ? AND name = ?`,
			ac.TenantID, name).Scan(&stackID); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_stack_lookup",
				map[string]any{"err": err.Error()})
			return
		}
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeStackActivated,
			ac.TenantID, stackID,
			req.VersionNumber, fleetBaseVersion,
			fleetArtifactRef, fleetChecksum,
		); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue",
				map[string]any{"err": qerr.Error()})
			return
		}
		// Propagate the auto-minted delegated-zone routing host (if this tenant
		// has a delegated zone, materialiseStackVersion just minted
		// <label>.<origin>). The dns_zones row isn't fleet-synced, so a
		// data-plane node can't re-derive it from this stack.activated replay —
		// ship the row so it can route + cert the host. See dns_fleet.go.
		if qerr := c.queueZoneHostnameUpserts(r.Context(), tx, ac.TenantID, name); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_zone_hostname",
				map[string]any{"err": qerr.Error()})
			return
		}
		// Same for the auto-minted structured-host-suffix routing host
		// (<stack>-<rand>.<suffix>). materialiseStackVersion minted it in this
		// tx; without shipping the row, only the control-plane node could route
		// the structured URL — every data-plane node 404s it (they no longer
		// re-mint, post mintHosts gate). See queueStructuredHostnameUpserts.
		if qerr := c.queueStructuredHostnameUpserts(r.Context(), tx, ac.TenantID, name); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_structured_hostname",
				map[string]any{"err": qerr.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	// Refresh the in-memory dbcache so this node serves the new active
	// version. Do it ASYNC: the activation is already durably committed and
	// the data plane has it via the control event published above, so
	// blocking the HTTP response on the multi-second dump+replay only times
	// out at the edge (502) with no correctness benefit. The FS watcher is
	// the backstop if this reload errors. Trade-off: this node's in-memory
	// mirror may lag the commit by one reload cycle; acceptable since the
	// commit is durable and the fleet event is the source of truth.
	go func() {
		if err := c.pu.Dbc.Reload(); err != nil {
			c.pu.Logger.Warn("async dbcache reload after activate failed; FS watcher will retry",
				zap.String("err", err.Error()))
		}
	}()

	// Materialise declarative store-seed packs (VECTORS/, KV/) into the runtime
	// stores — post-commit + async, like the reload above: the packs are durable
	// in the CAS and the reconcile does store I/O that must not block the
	// activate response. origin=true (this is the control plane), so shared
	// stores (pgvector) are reconciled here while data-plane appliers skip them.
	// r.Context() is canceled when the handler returns, so detach cancellation
	// (the tenant scope is passed explicitly; no request values are needed).
	// priorVersion = the version that was active before this flip, so reconcile
	// only touches packs that actually changed (a code deploy changes none).
	if c.storeReconciler != nil {
		var priorVersion int64
		if currentActiveID.Valid && currentActiveID.Int64 != targetVersionID {
			if pn, perr := c.versionNumberFor(r.Context(), currentActiveID.Int64); perr == nil {
				priorVersion = pn
			}
		}
		ssCtx := context.WithoutCancel(r.Context())
		go c.ReconcileStorePacks(ssCtx, ac.TenantID, name, req.VersionNumber, priorVersion, true)
	}

	resp := activateResponse{VersionNumber: req.VersionNumber}
	if currentActiveID.Valid && currentActiveID.Int64 != targetVersionID {
		priorN, err := c.versionNumberFor(r.Context(), currentActiveID.Int64)
		if err == nil {
			resp.PriorVersionNumber = &priorN
		}
	}

	// Surface the auto-minted hostname (read back post-commit; the mint
	// rode the activation tx). Best-effort: a skipped/failed mint just
	// leaves the URL empty — it never affects the activation result.
	if c.pu.Conf.StructuredHostSuffix != "" {
		var sh string
		if err := c.pu.RuntimeDB.QueryRowContext(r.Context(),
			`SELECT hostname FROM tenant_hostnames
			   WHERE tenant_id=? AND stack=? AND created_by=? AND revoked_at IS NULL
			   LIMIT 1`,
			ac.TenantID, name, tenants.SystemStructuredHostCreatedBy).Scan(&sh); err == nil && sh != "" {
			resp.StructuredURL = structuredURL(r, sh, c.pu.Conf.WebAddr)
		}
	}

	c.pu.Logger.Info("stack activated",
		zap.String("tenant", ac.TenantID),
		zap.String("stack", name),
		zap.Int64("version", req.VersionNumber),
		zap.String("user", authedUser(r)),
		zap.String("url", resp.StructuredURL))

	writeJSON(w, http.StatusOK, resp)
}

// handleValidateVersion: POST /v1/tenants/{t}/stacks/{name}/versions/{n}/validate
//
// Parses every `<scope>/<name>.txcl` file in the version through
// txcl.Resonator and reports per-file errors. Non-txcl files
// (mock-request.json, mock-response.json) are not parsed — they're
// payload fixtures, not rules. The endpoint is non-mutating; CI can
// call it on a draft before deciding whether to activate.
//
// 200 with `{"ok": true}` if all txcl files parse, regardless of how
// many files were checked (an empty draft is technically valid).
// 200 with `{"ok": false, "errors": [...]}` if any parse fails — the
// caller decides whether that's a hard fail or a warning. We don't
// return 422 here because the version itself isn't a malformed
// request; it's a query about the version's state.
func (c *Controller) handleValidateVersion(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	vars := mux.Vars(r)
	name := vars["name"]
	n, err := strconv.ParseInt(vars["n"], 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_version_number", nil)
		return
	}

	stackID, _, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_stack", map[string]any{"err": err.Error()})
		return
	}
	versionID, _, err := c.lookupVersion(r.Context(), c.pu.RuntimeDB, stackID, n)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "version_not_found", map[string]any{"version_number": n})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_version", map[string]any{"err": err.Error()})
		return
	}

	files, err := c.loadVersionFiles(r.Context(), versionID, contentAll)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "file_load_failed", map[string]any{"err": err.Error()})
		return
	}

	resp := validateResponse{OK: true}
	for _, f := range files {
		if !strings.HasSuffix(f.Path, ".txcl") {
			continue
		}
		resp.Checked++
		// Unresolved `op://NAME` parses fine as txcl (the scheme is only
		// checked at runtime dispatch) but can never run — surface it as
		// a validation error so the admin UI shows it before activate.
		if oprefs.HasRefs(f.Content) {
			resp.OK = false
			resp.Errors = append(resp.Errors, validateError{
				Path: f.Path,
				Err: "unresolved op://" + strings.Join(oprefs.References(f.Content), ", op://") +
					" — define it under operations: in txco.yaml and run `txco apply`, or use an explicit http(s):// URL",
			})
			continue
		}
		// Strict validation: catches unterminated strings, unknown
		// verbs, and trailing garbage that the lenient runtime parser
		// (txcl.Resonator) silently tolerates. Authoring-time only —
		// the runtime stays lenient so already-deployed rules don't
		// suddenly fail. One entry per diagnostic so the UI reports an
		// accurate count.
		for _, msg := range txcl.Validate(f.Content) {
			resp.OK = false
			resp.Errors = append(resp.Errors, validateError{Path: f.Path, Err: msg})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDiffVersions: GET /v1/tenants/{t}/stacks/{name}/diff?v1=&v2=
//
// Compares two versions of the same stack by content_hash per path.
// Same hash → omitted from the response. Different hashes → "changed".
// Path only in v2 → "added". Path only in v1 → "removed". When the
// versions' manifest_hash already match we short-circuit with
// `equal=true` and an empty entry list (the cheap-skip the design doc
// called out).
func (c *Controller) handleDiffVersions(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return
	}
	name := mux.Vars(r)["name"]
	v1, err1 := strconv.ParseInt(r.URL.Query().Get("v1"), 10, 64)
	v2, err2 := strconv.ParseInt(r.URL.Query().Get("v2"), 10, 64)
	if err1 != nil || err2 != nil || v1 <= 0 || v2 <= 0 {
		writeJSONError(w, http.StatusBadRequest, "bad_version_query",
			map[string]any{"hint": "both ?v1 and ?v2 must be positive integers"})
		return
	}

	stackID, _, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_stack", map[string]any{"err": err.Error()})
		return
	}

	type versionMeta struct {
		id           int64
		manifestHash string
	}
	loadMeta := func(versionNumber int64) (versionMeta, error) {
		var m versionMeta
		err := c.pu.RuntimeDB.QueryRowContext(r.Context(),
			`SELECT version_id, manifest_hash FROM stack_versions
			  WHERE stack_id = ? AND version_number = ?`,
			stackID, versionNumber).Scan(&m.id, &m.manifestHash)
		return m, err
	}
	m1, err := loadMeta(v1)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "version_not_found", map[string]any{"version_number": v1})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_v1", map[string]any{"err": err.Error()})
		return
	}
	m2, err := loadMeta(v2)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "version_not_found", map[string]any{"version_number": v2})
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_v2", map[string]any{"err": err.Error()})
		return
	}

	resp := diffResponse{V1: v1, V2: v2}
	// Manifest-hash short-circuit: when both sides have a non-empty
	// manifest hash and they match, the file sets are guaranteed
	// identical. Backfilled rows have an empty manifest hash, so we
	// fall through to the per-path scan in that case.
	if m1.manifestHash != "" && m1.manifestHash == m2.manifestHash {
		resp.Equal = true
		writeJSON(w, http.StatusOK, resp)
		return
	}

	loadHashes := func(versionID int64) (map[string]string, error) {
		rows, err := c.pu.RuntimeDB.QueryContext(r.Context(),
			`SELECT path, content, content_hash FROM stack_files WHERE version_id = ?`, versionID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var path, content, hash string
			if err := rows.Scan(&path, &content, &hash); err != nil {
				return nil, err
			}
			if hash == "" {
				hash = sha256Hex(content)
			}
			out[path] = hash
		}
		return out, nil
	}
	h1, err := loadHashes(m1.id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load_v1_files", map[string]any{"err": err.Error()})
		return
	}
	h2, err := loadHashes(m2.id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load_v2_files", map[string]any{"err": err.Error()})
		return
	}

	seen := map[string]bool{}
	for path, from := range h1 {
		seen[path] = true
		to, present := h2[path]
		switch {
		case !present:
			resp.Entries = append(resp.Entries, diffEntry{Path: path, Change: "removed", FromHash: from})
		case from != to:
			resp.Entries = append(resp.Entries, diffEntry{Path: path, Change: "changed", FromHash: from, ToHash: to})
		}
	}
	for path, to := range h2 {
		if seen[path] {
			continue
		}
		resp.Entries = append(resp.Entries, diffEntry{Path: path, Change: "added", ToHash: to})
	}
	sort.Slice(resp.Entries, func(i, j int) bool { return resp.Entries[i].Path < resp.Entries[j].Path })
	resp.Equal = len(resp.Entries) == 0
	writeJSON(w, http.StatusOK, resp)
}

// resolveDraftForMutation handles the common preamble of PATCH and
// DELETE: tenant resolve → path validate → stack lookup → version
// lookup → status=='draft' check. Writes the appropriate error
// response and returns (0, "", false) when any step fails; callers
// just `return` on a false result. On success returns the version_id
// and the validated path (a defensive copy of req's path is sometimes
// useful but for these handlers we just pass-through the original).
func (c *Controller) resolveDraftForMutation(w http.ResponseWriter, r *http.Request, reqPath string) (int64, bool) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return 0, false
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_unresolved", nil)
		return 0, false
	}
	if err := validateStackFilePath(reqPath); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_path",
			map[string]any{"path": reqPath, "reason": err.Error()})
		return 0, false
	}
	vars := mux.Vars(r)
	name := vars["name"]
	n, err := strconv.ParseInt(vars["n"], 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_version_number", nil)
		return 0, false
	}
	stackID, _, err := c.lookupStack(r.Context(), c.pu.RuntimeDB, ac.TenantID, name)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "stack_not_found", map[string]any{"name": name})
		return 0, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_stack", map[string]any{"err": err.Error()})
		return 0, false
	}
	versionID, status, err := c.lookupVersion(r.Context(), c.pu.RuntimeDB, stackID, n)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "version_not_found", map[string]any{"version_number": n})
		return 0, false
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "lookup_version", map[string]any{"err": err.Error()})
		return 0, false
	}
	if status != "draft" {
		writeJSONError(w, http.StatusConflict, "version_not_draft", map[string]any{"status": status})
		return 0, false
	}
	return versionID, true
}

// handlePatchDraftFile: PATCH /v1/tenants/{t}/stacks/{name}/versions/{n}/files
//
// Single-file upsert on a draft version with `base_hash` optimistic
// concurrency. Status codes follow the 404-vs-409 discipline:
//
//   - 404 when the stack, version, or named file path isn't present
//   - 409 when the resource exists but the caller's view is stale
//     (`base_hash` mismatch, version no longer draft, create-collision)
//   - 400 for malformed requests and path-validation failures
//
// On success, recomputes `stack_versions.manifest_hash` so the diff
// endpoint's short-circuit stays valid.
func (c *Controller) handlePatchDraftFile(w http.ResponseWriter, r *http.Request) {
	var req patchFileRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
		return
	}

	versionID, ok := c.resolveDraftForMutation(w, r, req.Path)
	if !ok {
		return
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Look up the current row for the named path. NoRows here means
	// the file doesn't exist in this version's file set.
	var (
		curContent string
		curHash    string
	)
	row := tx.QueryRowContext(r.Context(),
		`SELECT content, content_hash FROM stack_files WHERE version_id = ? AND path = ?`,
		versionID, req.Path)
	switch err := row.Scan(&curContent, &curHash); {
	case errors.Is(err, sql.ErrNoRows):
		// PATCH a missing file: only legal if the caller is creating
		// (base_hash == ""). Anything else means the caller thought
		// they were updating something that's already gone.
		if req.BaseHash != "" {
			writeJSONError(w, http.StatusNotFound, "file_not_found",
				map[string]any{"path": req.Path})
			return
		}
		newHash := sha256Hex(req.Content)
		if _, err := tx.ExecContext(r.Context(),
			`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (?, ?, ?, ?)`,
			versionID, req.Path, req.Content, newHash); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "insert_file", map[string]any{"err": err.Error()})
			return
		}
		curHash = newHash
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_file", map[string]any{"err": err.Error()})
		return
	default:
		// Row exists. Lazily compute hash for backfilled rows so a
		// caller passing GetVersion's surface hash matches.
		if curHash == "" {
			curHash = sha256Hex(curContent)
		}
		switch {
		case req.BaseHash == "":
			writeJSONError(w, http.StatusConflict, "file_already_exists",
				map[string]any{"path": req.Path, "current_hash": curHash})
			return
		case req.BaseHash != curHash:
			writeJSONError(w, http.StatusConflict, "base_hash_mismatch",
				map[string]any{"path": req.Path, "current_hash": curHash})
			return
		}
		newHash := sha256Hex(req.Content)
		if _, err := tx.ExecContext(r.Context(),
			`UPDATE stack_files SET content = ?, content_hash = ? WHERE version_id = ? AND path = ?`,
			req.Content, newHash, versionID, req.Path); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "update_file", map[string]any{"err": err.Error()})
			return
		}
		curHash = newHash
	}

	mhash, err := c.recomputeManifestHash(r.Context(), tx, versionID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "recompute_manifest", map[string]any{"err": err.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	writeJSON(w, http.StatusOK, patchFileResponse{
		Path:         req.Path,
		ContentHash:  curHash,
		ManifestHash: mhash,
	})
}

// handleDeleteDraftFile: DELETE /v1/tenants/{t}/stacks/{name}/versions/{n}/files
//
// Removes a single file from a draft. Requires a non-empty `base_hash`
// — no blind-delete mode, since blind writes against versioned state
// are the exact race this design is built to prevent. Recomputes the
// version's manifest_hash like PATCH does.
func (c *Controller) handleDeleteDraftFile(w http.ResponseWriter, r *http.Request) {
	var req deleteFileRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json", map[string]any{"err": err.Error()})
		return
	}
	if req.BaseHash == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_base_hash",
			map[string]any{"hint": "DELETE requires base_hash; refusing to delete without optimistic concurrency"})
		return
	}

	versionID, ok := c.resolveDraftForMutation(w, r, req.Path)
	if !ok {
		return
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var (
		curContent string
		curHash    string
	)
	row := tx.QueryRowContext(r.Context(),
		`SELECT content, content_hash FROM stack_files WHERE version_id = ? AND path = ?`,
		versionID, req.Path)
	switch err := row.Scan(&curContent, &curHash); {
	case errors.Is(err, sql.ErrNoRows):
		writeJSONError(w, http.StatusNotFound, "file_not_found", map[string]any{"path": req.Path})
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "lookup_file", map[string]any{"err": err.Error()})
		return
	}
	if curHash == "" {
		curHash = sha256Hex(curContent)
	}
	if req.BaseHash != curHash {
		writeJSONError(w, http.StatusConflict, "base_hash_mismatch",
			map[string]any{"path": req.Path, "current_hash": curHash})
		return
	}

	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM stack_files WHERE version_id = ? AND path = ?`,
		versionID, req.Path); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "delete_file", map[string]any{"err": err.Error()})
		return
	}

	mhash, err := c.recomputeManifestHash(r.Context(), tx, versionID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "recompute_manifest", map[string]any{"err": err.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	writeJSON(w, http.StatusOK, deleteFileResponse{
		Path:         req.Path,
		Deleted:      true,
		ManifestHash: mhash,
	})
}
