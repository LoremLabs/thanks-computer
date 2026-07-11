package admin

// Fleet propagation for delegated-zone routing hostnames.
//
// A pattern-mode delegated zone auto-mints a verified tenant_hostnames row
// `<StackLabel(stack)>.<origin>` per active stack (tenants.EnsureZoneHostnameTx,
// created_by = SystemZoneHostCreatedBy). That row is what a chassis routes on
// AND what the on-demand-TLS `ask` gate checks — so EVERY node behind the LB
// needs it, not just the admin node.
//
// The dns_zones row itself is NOT fleet-synced yet (see dns_crud_endpoints.go's
// "Fleet note"), so a data-plane node replaying a stack.activated event can't
// re-derive the delegated-zone host — its local mint sees no zone and falls back
// to the structured-host suffix instead. The fix: ship the minted row directly,
// the same way explicit hostname CRUD does (fleet_resync.go / tenant_hostname_
// endpoints.go) — a content-addressed RowsArtifact upsert that the consumer's
// applyRows writes verbatim (id-stable, so later upserts stay idempotent).
//
// Two producers call queueZoneHostnameUpserts: zone create (reconcile every
// already-active stack) and stack activation (propagate the one just minted).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// EnsureStructuredSuffixZone idempotently creates the system-owned zone for the
// configured structured-host suffix (e.g. stacks.thanks.computer), making the
// chassis authoritative for it: synth emits a WILDCARD A/MX/SPF when a zone's
// origin == the suffix, and per-host DKIM/DMARC come from the structured-host
// rows. Reuses the zone create + fleet-publish + dbcache-reload path.
// Control-plane only (the caller gates on the 'admin' personality +
// --structured-dns-self). No-op when the zone already exists.
func (c *Controller) EnsureStructuredSuffixZone(ctx context.Context) error {
	if c.tenants == nil || c.pu == nil || c.pu.RuntimeDB == nil {
		return errors.New("structured-suffix zone: store not initialized (call after Start)")
	}
	suffix := normalizeSuffix(c.pu.Conf.StructuredHostSuffix)
	if suffix == "" {
		return nil
	}
	canon, ok := tenants.CanonicalizeHost(suffix)
	if !ok || !tenants.IsValidHostname(canon) {
		return fmt.Errorf("structured-suffix zone: invalid suffix %q", suffix)
	}
	if _, err := c.tenants.LookupActiveZone(ctx, tenants.SystemTenantID, canon); err == nil {
		return nil // already seeded
	} else if !errors.Is(err, tenants.ErrNotFound) {
		return err
	}
	ns := firstNameserver(c.pu.Conf.DNSNameservers)
	if ns == "" {
		return fmt.Errorf("structured-suffix zone: --dns-nameservers required to seed %q", canon)
	}
	z := tenants.DNSZone{
		ID:        tenants.NewZoneID(),
		TenantID:  tenants.SystemTenantID,
		Origin:    canon,
		MName:     ns,
		RName:     "hostmaster." + canon,
		Mode:      "pattern",
		CreatedBy: "system:structured-suffix",
	}
	tx, err := c.pu.RuntimeDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := c.tenants.CreateZoneTx(ctx, tx, z); err != nil {
		if errors.Is(err, tenants.ErrZoneExists) {
			return nil
		}
		return err
	}
	if c.fleetEnabled() {
		persisted, gerr := tenants.GetZoneByIDTx(ctx, tx, z.ID, c.pu.RuntimeDialect)
		if gerr != nil {
			return gerr
		}
		if err := c.fleetPublishZone(ctx, tx, persisted); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.pu.Logger.Info("seeded structured-suffix DNS zone", zap.String("origin", canon))
	return c.pu.Dbc.Reload()
}

// BackfillStructuredHostDKIM mints a per-host DKIM keypair for every active
// chassis-minted structured host that predates the per-host key columns (0017)
// — created_by = structured-host with an empty key. Idempotent: hosts that
// already have a key are skipped, so re-running (e.g. every boot) is a cheap
// no-op once the fleet is keyed. Each updated row is fleet-published so data-
// plane nodes sign with it and the dns head publishes its per-host records.
// Control-plane only. Returns the number of hosts newly keyed.
func (c *Controller) BackfillStructuredHostDKIM(ctx context.Context) (int, error) {
	if c.tenants == nil || c.pu == nil || c.pu.RuntimeDB == nil {
		return 0, errors.New("dkim backfill: store not initialized (call after Start)")
	}
	rows, err := c.pu.RuntimeDB.QueryContext(ctx,
		c.rb(`SELECT hostname FROM tenant_hostnames
		  WHERE created_by = ? AND revoked_at IS NULL AND dkim_private_pem = ''`),
		tenants.SystemStructuredHostCreatedBy)
	if err != nil {
		return 0, err
	}
	var hosts []string
	for rows.Next() {
		var h string
		if serr := rows.Scan(&h); serr != nil {
			rows.Close()
			return 0, serr
		}
		hosts = append(hosts, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	keyed := 0
	for _, hostname := range hosts {
		h, lerr := c.tenants.LookupActiveHostname(ctx, hostname)
		if lerr != nil {
			c.pu.Logger.Warn("dkim backfill: load host failed",
				zap.String("hostname", hostname), zap.Error(lerr))
			continue
		}
		if h.DKIMPrivatePEM != "" {
			continue // raced — already keyed
		}
		priv, pub, gerr := tenants.GenerateDKIM()
		if gerr != nil {
			return keyed, gerr
		}
		h.DKIMSelector, h.DKIMPrivatePEM, h.DKIMPublicB64 = tenants.DKIMSelector, priv, pub

		var ref, sum string
		if c.fleetEnabled() {
			r, s, ferr := c.fleetUploadHostnameUpsert(ctx, h)
			if ferr != nil {
				return keyed, ferr
			}
			ref, sum = r, s
		}
		tx, terr := c.pu.RuntimeDB.BeginTx(ctx, nil)
		if terr != nil {
			return keyed, terr
		}
		if _, uerr := tx.ExecContext(ctx,
			c.rb(`UPDATE tenant_hostnames SET dkim_selector = ?, dkim_private_pem = ?, dkim_public_b64 = ?
			  WHERE id = ?`),
			h.DKIMSelector, h.DKIMPrivatePEM, h.DKIMPublicB64, h.ID); uerr != nil {
			_ = tx.Rollback()
			return keyed, uerr
		}
		if c.fleetEnabled() {
			if _, qerr := c.fleetQueueEvent(ctx, tx,
				controlevent.TypeHostnameBound, h.TenantID, "", 0, 0, ref, sum); qerr != nil {
				_ = tx.Rollback()
				return keyed, qerr
			}
		}
		if cerr := tx.Commit(); cerr != nil {
			return keyed, cerr
		}
		keyed++
	}
	if keyed > 0 {
		c.pu.Logger.Info("backfilled per-host DKIM keys for structured hosts", zap.Int("count", keyed))
		if rerr := c.pu.Dbc.Reload(); rerr != nil {
			return keyed, rerr
		}
	}
	return keyed, nil
}

// normalizeSuffix lowercases + strips a leading "." and trailing "." from the
// structured-host suffix so it can be compared to a canonical zone origin.
func normalizeSuffix(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(strings.TrimPrefix(s, "."), ".")
}

// firstNameserver returns the first configured DNS nameserver (handling the
// comma-CSV-in-one-element env quirk), or "".
func firstNameserver(in []string) string {
	for _, e := range in {
		for _, p := range strings.Split(e, ",") {
			if t := strings.TrimSpace(p); t != "" {
				return t
			}
		}
	}
	return ""
}

// zoneHostTSLayout matches the RFC3339-UTC text the tenant_hostnames row
// serializer (hostnameToRow) emits, so a minted-then-published row round-trips
// to the consumer byte-identically.
const zoneHostTSLayout = "2006-01-02T15:04:05Z"

// activeMintableStacks returns the tenant's active, non-system stack names —
// the set that gets a `<label>.<origin>` host synthesized + routed. Read from
// the passed tx so just-committed-in-tx state is visible. Mirrors the dns
// head's synthesis filter (isSynthesizableStack) via isMintableStack.
func (c *Controller) activeMintableStacks(ctx context.Context, tx *sql.Tx, tenantID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		c.rb(`SELECT name FROM stacks WHERE tenant_id = ? AND active_version IS NOT NULL`), tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if isMintableStack(name) {
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

// reconcileZoneHostnames mints the routing host for every active stack of the
// tenant under origin (idempotent), then fleet-publishes them. Called from zone
// create so a zone created AFTER its stacks were activated still wires them up —
// the activation-time mint only fires when the zone already exists. A single
// mint failure is logged and skipped (it must never fail the zone create, like
// the activation path); a fleet-publish failure is returned (atomic with the tx).
func (c *Controller) reconcileZoneHostnames(ctx context.Context, tx *sql.Tx, tenantID, origin string) error {
	now := time.Now().UTC().Format(zoneHostTSLayout)
	stacks, err := c.activeMintableStacks(ctx, tx, tenantID)
	if err != nil {
		return fmt.Errorf("load active stacks: %w", err)
	}
	for _, s := range stacks {
		if _, merr := tenants.EnsureZoneHostnameTx(ctx, tx, tenantID, s, origin, now, c.pu.RuntimeDialect); merr != nil {
			c.pu.Logger.Warn("zone-create reconcile: hostname mint skipped (zone create unaffected)",
				zap.String("tenant", tenantID), zap.String("stack", s),
				zap.String("origin", origin), zap.String("err", merr.Error()))
		}
	}
	return c.queueZoneHostnameUpserts(ctx, tx, tenantID, "")
}

// queueZoneHostnameUpserts fleet-publishes the tenant's delegated-zone routing
// hostnames (created_by = SystemZoneHostCreatedBy) as TypeHostnameBound row
// upserts — all of them, or just one stack's when stack != "". Rows are read
// from tx so a same-tx mint is visible. No-op when fleet sync is off.
//
// Artifact-before-outbox ordering holds: the Put precedes the in-tx outbox
// append, so an accepted DB mutation never lacks its artifact (a Put whose tx
// later rolls back just orphans the artifact, which the sweeper GCs).
func (c *Controller) queueZoneHostnameUpserts(ctx context.Context, tx *sql.Tx, tenantID, stack string) error {
	pending, err := c.collectZoneHostUpserts(ctx, tx, tenantID, stack)
	if err != nil {
		return err
	}
	return c.publishHostEventsInTx(ctx, tx, pending)
}

// pendingHostEvent is one hostname RowsArtifact collected inside a tx whose
// R2 upload + outbox append can be deferred past commit. The activation tx
// holds the stacks FOR UPDATE row (and, on shared Postgres, a pooled
// connection) — an R2 round trip inside that window blocks every concurrent
// same-stack operation for the full R2 timeout, so the activation path
// collects in-tx (the freshly-minted row is only visible there) and
// publishes AFTER commit via publishHostEvents. The small direct callers
// (zone create/verify, hostname mint) still publish in-tx via
// publishHostEventsInTx pending their own batch rework.
type pendingHostEvent struct {
	tenantID string
	key      string // artifact key: rows/tenant_hostnames/<id>
	art      controlevent.RowsArtifact
}

// collectZoneHostUpserts reads the delegated-zone routing-host rows
// (created_by = system:zone-host) from tx — all of the tenant's, or just one
// stack's — and returns them as pending one-row upsert artifacts. SQL only;
// no uploads. Empty when fleet sync is off.
func (c *Controller) collectZoneHostUpserts(ctx context.Context, tx *sql.Tx, tenantID, stack string) ([]pendingHostEvent, error) {
	if !c.fleetEnabled() {
		return nil, nil
	}
	q := `SELECT id, hostname, tenant_id, stack, created_at, created_by, verified_at
	        FROM tenant_hostnames
	       WHERE tenant_id = ? AND created_by = ? AND revoked_at IS NULL`
	args := []any{tenantID, tenants.SystemZoneHostCreatedBy}
	if stack != "" {
		q += ` AND stack = ?`
		args = append(args, stack)
	}
	rows, err := tx.QueryContext(ctx, c.rb(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []pendingHostEvent
	for rows.Next() {
		var (
			id, hostname, tid, stk, createdAt, createdBy string
			verifiedAt                                   sql.NullString
		)
		if err := rows.Scan(&id, &hostname, &tid, &stk, &createdAt, &createdBy, &verifiedAt); err != nil {
			return nil, err
		}
		row := map[string]any{
			"id":         id,
			"hostname":   hostname,
			"tenant_id":  tid,
			"stack":      stk,
			"created_at": createdAt,
			"created_by": createdBy,
		}
		if verifiedAt.Valid && verifiedAt.String != "" {
			row["verified_at"] = verifiedAt.String
		}
		pending = append(pending, pendingHostEvent{
			tenantID: tid,
			key:      fmt.Sprintf("rows/tenant_hostnames/%s", id),
			art: controlevent.RowsArtifact{
				DB:    "runtime",
				Table: "tenant_hostnames",
				Op:    "upsert",
				Rows:  []map[string]any{row},
			},
		})
	}
	return pending, rows.Err()
}

// publishHostEventsInTx uploads each pending artifact and appends its
// hostname.bound event to the SAME tx — the pre-refactor behavior, kept for
// the low-volume direct callers. NOTE: this spans R2 round trips inside the
// caller's tx; do not add new callers — collect + publishHostEvents instead.
func (c *Controller) publishHostEventsInTx(ctx context.Context, tx *sql.Tx, pending []pendingHostEvent) error {
	for _, p := range pending {
		ref, sum, _, err := c.fleetUploadArtifact(ctx, p.key, p.art)
		if err != nil {
			return fmt.Errorf("upload %s: %w", p.key, err)
		}
		if _, err := c.fleetQueueEvent(ctx, tx,
			controlevent.TypeHostnameBound, p.tenantID, "", 0, 0, ref, sum); err != nil {
			return fmt.Errorf("queue %s: %w", p.key, err)
		}
	}
	return nil
}

// publishHostEvents is the post-commit half of the collect/publish split:
// upload every pending artifact, THEN append all the hostname.bound events
// in one short fresh tx — upload-before-append holds per event, so a
// committed event never lacks its artifact. A crash between the caller's
// commit and this publish leaves the mutation without its fleet event —
// SQLite-fleet drift only (shared-Postgres nodes read the rows directly and
// reload on the accompanying stack.activated event), healed by
// `txco fleet resync`.
func (c *Controller) publishHostEvents(ctx context.Context, pending []pendingHostEvent) error {
	if len(pending) == 0 || !c.fleetEnabled() {
		return nil
	}
	type uploaded struct {
		p        pendingHostEvent
		ref, sum string
	}
	ups := make([]uploaded, 0, len(pending))
	for _, p := range pending {
		ref, sum, _, err := c.fleetUploadArtifact(ctx, p.key, p.art)
		if err != nil {
			return fmt.Errorf("upload %s: %w", p.key, err)
		}
		ups = append(ups, uploaded{p: p, ref: ref, sum: sum})
	}
	tx, err := c.pu.RuntimeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin host-event tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for _, u := range ups {
		if _, err := c.fleetQueueEvent(ctx, tx,
			controlevent.TypeHostnameBound, u.p.tenantID, "", 0, 0, u.ref, u.sum); err != nil {
			return fmt.Errorf("queue %s: %w", u.p.key, err)
		}
	}
	return tx.Commit()
}

// queueStructuredHostnameUpserts ships the auto-minted structured-host-suffix
// routing host(s) (created_by = system:structured-host) the same way
// queueZoneHostnameUpserts ships delegated-zone hosts: a tenant_hostnames
// RowsArtifact (upsert) + a hostname.bound event, inside the activation tx.
// Without it, the host minted on the control plane never reaches data-plane
// nodes, so they 404 the `<stack>-<rand>.<suffix>` URL — the mint is now
// control-plane-only (materialiseStackVersion's mintHosts gate).
//
// Unlike the zone projection, this carries the per-host DKIM columns. A
// structured host signs `d=<host>` with its own key, and the consumer applies
// rows with INSERT OR REPLACE (upsertRow), which would blank any omitted
// NOT-NULL-DEFAULT-” column. Mirrors hostnameToRow's column set so the key
// survives on data-plane nodes.
func (c *Controller) queueStructuredHostnameUpserts(ctx context.Context, tx *sql.Tx, tenantID, stack string) error {
	pending, err := c.collectStructuredHostUpserts(ctx, tx, tenantID, stack)
	if err != nil {
		return err
	}
	return c.publishHostEventsInTx(ctx, tx, pending)
}

// collectStructuredHostUpserts is collectZoneHostUpserts for the
// structured-suffix hosts (created_by = system:structured-host), carrying
// the per-host DKIM columns the consumer's INSERT OR REPLACE must not blank.
// SQL only; no uploads. Empty when fleet sync is off.
func (c *Controller) collectStructuredHostUpserts(ctx context.Context, tx *sql.Tx, tenantID, stack string) ([]pendingHostEvent, error) {
	if !c.fleetEnabled() {
		return nil, nil
	}
	q := `SELECT id, hostname, tenant_id, stack, created_at, created_by, verified_at,
	             dkim_selector, dkim_private_pem, dkim_public_b64
	        FROM tenant_hostnames
	       WHERE tenant_id = ? AND created_by = ? AND revoked_at IS NULL`
	args := []any{tenantID, tenants.SystemStructuredHostCreatedBy}
	if stack != "" {
		q += ` AND stack = ?`
		args = append(args, stack)
	}
	rows, err := tx.QueryContext(ctx, c.rb(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []pendingHostEvent
	for rows.Next() {
		var (
			id, hostname, tid, stk, createdAt, createdBy string
			dkimSelector, dkimPrivatePEM, dkimPublicB64  string
			verifiedAt                                   sql.NullString
		)
		if err := rows.Scan(&id, &hostname, &tid, &stk,
			&createdAt, &createdBy, &verifiedAt,
			&dkimSelector, &dkimPrivatePEM, &dkimPublicB64); err != nil {
			return nil, err
		}
		row := map[string]any{
			"id":               id,
			"hostname":         hostname,
			"tenant_id":        tid,
			"stack":            stk,
			"created_at":       createdAt,
			"created_by":       createdBy,
			"dkim_selector":    dkimSelector,
			"dkim_private_pem": dkimPrivatePEM,
			"dkim_public_b64":  dkimPublicB64,
		}
		if verifiedAt.Valid && verifiedAt.String != "" {
			row["verified_at"] = verifiedAt.String
		}
		pending = append(pending, pendingHostEvent{
			tenantID: tid,
			key:      fmt.Sprintf("rows/tenant_hostnames/%s", id),
			art: controlevent.RowsArtifact{
				DB:    "runtime",
				Table: "tenant_hostnames",
				Op:    "upsert",
				Rows:  []map[string]any{row},
			},
		})
	}
	return pending, rows.Err()
}

// zoneToRow projects a DNSZone onto the JSON-row shape the consumer's
// applyRows upserts into dns_zones (INSERT OR REPLACE; the partial-unique on
// active origin dedups). All NOT-NULL columns are always present; created_by /
// revoked_at are omitted when empty so the consumer writes NULL, not "".
func zoneToRow(z tenants.DNSZone) map[string]any {
	row := map[string]any{
		"id":          z.ID,
		"tenant_id":   z.TenantID,
		"origin":      z.Origin,
		"mname":       z.MName,
		"rname":       z.RName,
		"refresh":     z.Refresh,
		"retry":       z.Retry,
		"expire":      z.Expire,
		"minimum":     z.Minimum,
		"default_ttl": z.DefaultTTL,
		"mode":        z.Mode,
		"created_at":  z.CreatedAt,
		"updated_at":  z.UpdatedAt,
		// DKIM material (0016) — NOT NULL DEFAULT '', so always carried; a
		// later upsert must not blank it out on data-plane nodes.
		"dkim_selector":    z.DKIMSelector,
		"dkim_private_pem": z.DKIMPrivatePEM,
		"dkim_public_b64":  z.DKIMPublicB64,
	}
	if z.CreatedBy != "" {
		row["created_by"] = z.CreatedBy
	}
	if z.RevokedAt != "" {
		row["revoked_at"] = z.RevokedAt
	}
	// verified_at (0019) gates serving on data-plane nodes (BuildSnapshot), so it
	// MUST ride the fleet upsert — else a synced zone reads pending and stops
	// serving. We only ever publish VERIFIED zones (create-when-verified +
	// verify), so it's always set here; the consumer's INSERT OR REPLACE would
	// otherwise reset an absent column to pending.
	if z.VerifiedAt != "" {
		row["verified_at"] = z.VerifiedAt
	}
	return row
}

// fleetPublishZone uploads a dns_zones row artifact and queues a
// TypeDNSZoneUpserted event in tx. No-op when fleet sync is off. Revocation is
// just an upsert of the same row with revoked_at set (the consumer's INSERT OR
// REPLACE flips it inactive). The artifact key is id-keyed so retries overwrite.
func (c *Controller) fleetPublishZone(ctx context.Context, tx *sql.Tx, z tenants.DNSZone) error {
	if !c.fleetEnabled() {
		return nil
	}
	art := controlevent.RowsArtifact{
		DB:    "runtime",
		Table: "dns_zones",
		Op:    "upsert",
		Rows:  []map[string]any{zoneToRow(z)},
	}
	ref, sum, _, err := c.fleetUploadArtifact(ctx, fmt.Sprintf("rows/dns_zones/%s", z.ID), art)
	if err != nil {
		return fmt.Errorf("upload dns zone %s: %w", z.ID, err)
	}
	if _, err := c.fleetQueueEvent(ctx, tx,
		controlevent.TypeDNSZoneUpserted, z.TenantID, "", 0, 0, ref, sum); err != nil {
		return fmt.Errorf("queue dns zone %s: %w", z.ID, err)
	}
	return nil
}
