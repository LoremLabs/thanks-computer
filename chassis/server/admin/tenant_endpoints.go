package admin

// Tenant CRUD + membership listing. These endpoints sit alongside
// the tenant-scoped /v1/tenants/{t}/... subrouter:
//
//	GET  /v1/tenants            list tenants the caller can see
//	POST /v1/tenants            create a new tenant (super_admin only)
//	GET  /v1/tenants/{t}/auth/members
//	                            list members of one tenant (admin in t)
//
// The CRUD pair is chassis-wide; the member list is tenant-scoped so
// it inherits resolveTenantMiddleware's slug→id resolution and the
// "actor:read"-in-this-tenant capability check.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

type tenantRecord struct {
	TenantID  string `json:"tenant_id"`
	Slug      string `json:"slug"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"created_at"`
}

type listTenantsResponse struct {
	Tenants []tenantRecord `json:"tenants"`
}

// handleListTenants returns the set of tenants the caller can see:
// super_admin gets every tenant; everyone else gets only tenants in
// which they have an active membership. Sorted alphabetically by
// slug for stable shell-script consumption.
func (c *Controller) handleListTenants(w http.ResponseWriter, r *http.Request) {
	ac := auth.FromContext(r.Context())
	if ac == nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}

	var rows []tenants.Tenant
	var err error

	// super_admin and non-signed callers (basic-auth/open) see every
	// tenant. The matching shape is `RequireSuperAdmin` — same trust
	// level as creating tenants.
	if policy.RequireSuperAdmin(r.Context()) == nil {
		rows, err = c.tenants.List(r.Context())
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "list_tenants",
				map[string]any{"err": err.Error()})
			return
		}
	} else {
		// Signed actor: derive from memberships. One query, one row
		// per (actor, tenant). We re-lookup tenant rows so revoked
		// tenants are filtered out cleanly.
		ms, mErr := c.registry.ListMembershipsForActor(r.Context(), ac.ActorID)
		if mErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "list_memberships",
				map[string]any{"err": mErr.Error()})
			return
		}
		for _, m := range ms {
			t, err := c.tenants.Lookup(r.Context(), m.TenantID)
			if err != nil || t == nil || t.RevokedAt != nil {
				continue
			}
			rows = append(rows, *t)
		}
	}

	out := listTenantsResponse{Tenants: make([]tenantRecord, 0, len(rows))}
	for _, t := range rows {
		out.Tenants = append(out.Tenants, tenantRecord{
			TenantID:  t.TenantID,
			Slug:      t.Slug,
			Name:      t.Name,
			CreatedAt: t.CreatedAt.Format("2006-01-02T15:04:05Z"),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

type createTenantRequest struct {
	Slug string `json:"slug"`
	Name string `json:"name,omitempty"`
}

// handleCreateTenant mints a new tenant. Chassis-wide action — only
// super_admin (or basic-auth/open operator) can create tenants. The
// slug is the durable handle; tenant_id is generated server-side and
// returned. Duplicate slug collisions surface as 409.
func (c *Controller) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	var req createTenantRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body",
			map[string]any{"err": err.Error()})
		return
	}
	slug := strings.ToLower(strings.TrimSpace(req.Slug))
	if slug == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_slug",
			map[string]any{"hint": "slug is required and must be non-empty"})
		return
	}
	if tenants.ReservedSlug(slug) {
		writeJSONError(w, http.StatusBadRequest, "reserved_slug",
			map[string]any{"slug": slug, "hint": "the _ prefix is reserved for chassis-internal tenants"})
		return
	}

	// Refuse collisions up front so the response distinguishes "you
	// already have a tenant with this slug" from generic 500. The
	// UNIQUE constraint on tenants.slug would catch it too, but only
	// after the row is partially-inserted.
	if existing, err := c.tenants.LookupBySlug(r.Context(), slug); err == nil && existing != nil {
		writeJSONError(w, http.StatusConflict, "tenant_slug_taken",
			map[string]any{"slug": slug, "tenant_id": existing.TenantID})
		return
	}

	t := tenants.Tenant{
		TenantID:  "tnt_" + hxid.NewTimeSort().String(),
		Slug:      slug,
		Name:      req.Name,
		CreatedAt: time.Now().UTC(),
	}

	// Fleet-sync producer: when feed-sink is enabled, upload the
	// RowsArtifact BEFORE the tx + write the outbox row INSIDE the
	// tx. Single-node deploys skip every byte of this (fleetEnabled
	// returns false when --feed-sink=nop).
	var fleetArtifactRef, fleetChecksum string
	if c.fleetEnabled() {
		art := controlevent.RowsArtifact{
			DB:    "runtime",
			Table: "tenants",
			Op:    "upsert",
			Rows: []map[string]any{
				tenantToRow(t),
			},
		}
		key := fmt.Sprintf("rows/tenants/%s", t.TenantID)
		ref, sum, _, uerr := c.fleetUploadArtifact(r.Context(), key, art)
		if uerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_upload",
				map[string]any{"err": uerr.Error()})
			return
		}
		fleetArtifactRef, fleetChecksum = ref, sum
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

	if err := c.tenants.CreateTx(r.Context(), tx, t); err != nil {
		// The pre-check above races a concurrent create; the UNIQUE
		// backstop on tenants.slug catches the loser — surface it as the
		// same 409 the pre-check gives, not a 500.
		if c.dia().IsUniqueViolationGeneric(err) {
			writeJSONError(w, http.StatusConflict, "tenant_slug_taken",
				map[string]any{"slug": slug})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "create_tenant",
			map[string]any{"err": err.Error()})
		return
	}

	if c.fleetEnabled() {
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeTenantCreated, t.TenantID, "", 0, 0,
			fleetArtifactRef, fleetChecksum,
		); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue",
				map[string]any{"err": qerr.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	writeJSON(w, http.StatusOK, tenantRecord{
		TenantID:  t.TenantID,
		Slug:      t.Slug,
		Name:      t.Name,
		CreatedAt: t.CreatedAt.Format("2006-01-02T15:04:05Z"),
	})
}

// tenantToRow projects a Tenant onto the JSON-row shape the consumer
// applier uses for RowsArtifact upserts. Mirrors the column set of
// the tenants table (db/schema/sqlite/runtime/0002_tenants.sql).
func tenantToRow(t tenants.Tenant) map[string]any {
	row := map[string]any{
		"tenant_id":  t.TenantID,
		"slug":       t.Slug,
		"created_at": t.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if t.Name != "" {
		row["name"] = t.Name
	}
	return row
}

type memberRecord struct {
	ActorID      string   `json:"actor_id"`
	Label        string   `json:"label,omitempty"`
	Capabilities []string `json:"capabilities"`
	CreatedAt    string   `json:"created_at"`
}

type listTenantMembersResponse struct {
	Members []memberRecord `json:"members"`
}

type grantMemberRequest struct {
	ActorID      string   `json:"actor_id"`
	Capabilities []string `json:"capabilities"`
}

// handleListTenantMembers lists the members of the URL's tenant.
// Tenant-scoped: the tenant middleware has already resolved
// auth.Context.TenantID and gated the actor:read capability.
//
// Decorates each row with the actor's label so admin tools can render
// "alice / actor_xxx" without a second query per member.
func (c *Controller) handleListTenantMembers(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "actor:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	members, err := c.registry.ListMembershipsForTenant(r.Context(), ac.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_members",
			map[string]any{"err": err.Error()})
		return
	}
	out := listTenantMembersResponse{Members: make([]memberRecord, 0, len(members))}
	for _, m := range members {
		rec := memberRecord{
			ActorID:      m.ActorID,
			Capabilities: m.Capabilities,
			CreatedAt:    m.CreatedAt.Format("2006-01-02T15:04:05Z"),
		}
		if a, err := c.registry.LookupActor(r.Context(), m.ActorID); err == nil && a != nil {
			rec.Label = a.Label
		}
		out.Members = append(out.Members, rec)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGrantMember upserts a membership: alice in tenant t with the
// given capability set. Same trust level as creating an invitation
// (actor:*:invite in t), since this is the no-token-round-trip
// equivalent. The PRIMARY KEY (actor_id, tenant_id) on
// actor_memberships makes CreateMembership idempotent — re-granting
// replaces the capability set and clears any prior revocation.
func (c *Controller) handleGrantMember(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "actor:*:invite"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}

	var req grantMemberRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body",
			map[string]any{"err": err.Error()})
		return
	}
	if strings.TrimSpace(req.ActorID) == "" {
		writeJSONError(w, http.StatusBadRequest, "actor_id_required", nil)
		return
	}
	if len(req.Capabilities) == 0 {
		writeJSONError(w, http.StatusBadRequest, "capabilities_required",
			map[string]any{"hint": "pass at least one capability; an empty grant has no useful semantics"})
		return
	}
	// Canonicalise + validate every entry. Refuse unknowns up front
	// so the row never lands with a typo'd cap that gives the member
	// neither the role they expected nor a clear failure.
	caps, err := policy.ParseCapabilities(strings.Join(req.Capabilities, ","))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "unknown_capability",
			map[string]any{"err": err.Error()})
		return
	}

	// Anti-privilege-escalation: a signed, non-super-admin granter
	// can't issue capabilities they don't have themselves in this
	// tenant. ac.Capabilities was set by the tenant middleware from
	// the granter's membership row, so the check is "do my own grants
	// cover each requested cap?" super_admin and basic-auth/open
	// operators bypass.
	if ac.Source == "signed" && !ac.SuperAdmin {
		if missing := policy.CoversAll(ac.Capabilities, caps); missing != "" {
			writeJSONError(w, http.StatusForbidden, "capability_exceeds_granter",
				map[string]any{
					"denied_capability": missing,
					"hint":              "you cannot grant capabilities you don't have yourself in this tenant",
				})
			return
		}
	}

	// Refuse granting to a non-existent or revoked actor: clearer
	// than letting the FK INSERT explode, and avoids 500 noise.
	a, err := c.registry.LookupActor(r.Context(), req.ActorID)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			writeJSONError(w, http.StatusNotFound, "actor_not_found",
				map[string]any{"actor_id": req.ActorID})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "lookup_actor",
			map[string]any{"err": err.Error()})
		return
	}
	if a.RevokedAt != nil {
		writeJSONError(w, http.StatusBadRequest, "actor_revoked",
			map[string]any{"actor_id": req.ActorID})
		return
	}

	m, err := c.registry.CreateMembership(r.Context(), registry.Membership{
		ActorID:      req.ActorID,
		TenantID:     ac.TenantID,
		Capabilities: caps,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "grant_member",
			map[string]any{"err": err.Error()})
		return
	}

	rec := memberRecord{
		ActorID:      m.ActorID,
		Label:        a.Label,
		Capabilities: m.Capabilities,
		CreatedAt:    m.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
	writeJSON(w, http.StatusOK, rec)
}

// handleRevokeMember soft-deletes a membership. The actor and key
// rows are untouched — the principal keeps their other memberships
// and chassis-wide identity. Idempotent: revoking an absent or
// already-revoked membership is a no-op (200 with revoked=true).
func (c *Controller) handleRevokeMember(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "actor:*:invite"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	actorID := mux.Vars(r)["actorID"]
	if actorID == "" {
		writeJSONError(w, http.StatusBadRequest, "actor_id_required", nil)
		return
	}
	if err := c.registry.RevokeMembership(r.Context(), actorID, ac.TenantID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "revoke_member",
			map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revoked":   true,
		"actor_id":  actorID,
		"tenant_id": ac.TenantID,
	})
}
