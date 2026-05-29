-- Seed the reserved system tenant.
--
-- `_sys` owns the chassis ingress-fallback namespace (`boot/*`). A
-- request that matches no ingress route is stamped with this tenant
-- (chassis/server.dispatchEnvelope) and runs pinned to it, so the
-- data-plane op lookup is tenant-filtered like every other request —
-- this removes the former "untenanted == unfiltered global lookup"
-- carve-out entirely. Operator-authored boot rules are activated under
-- this tenant; a `_sys` boot rule may re-tenant a request into a real
-- tenant (the only place a request's pinned tenant may change, and
-- only one-way: _sys -> concrete, never the reverse).
--
-- The `_` slug prefix is reserved (chassis/tenants.ReservedSlug), so
-- no created tenant can ever collide with or impersonate `_sys`. This
-- INSERT bypasses the Go validation by writing the row directly, which
-- is the intended (and only) way the reserved slug enters the table.
INSERT OR IGNORE INTO tenants (tenant_id, slug, name, created_at)
VALUES ('tnt_sys', '_sys', 'System (ingress fallback)',
        strftime('%Y-%m-%dT%H:%M:%fZ','now'));
