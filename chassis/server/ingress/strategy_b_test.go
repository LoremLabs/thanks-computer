package ingress

import (
	"testing"
)

// ----- YAML stand-in (rule 4, in-process) -----

func TestStrategyB_YAMLVerifiedDomain_Convention(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    verified_domains:
      acme.example:
        tenant: acme
`
	mr := mailResolverFor(t, yaml)
	got, ok := mr.ResolveRecipient("anyone@acme.example", "")
	if !ok {
		t.Fatalf("expected hit")
	}
	if got.Tenant != "acme" {
		t.Errorf("tenant = %q, want %q", got.Tenant, "acme")
	}
	if got.Stack != "_mail" {
		t.Errorf("stack = %q, want %q (mail-only convention default)", got.Stack, "_mail")
	}
	if got.Ingress != "domain:acme.example" {
		t.Errorf("ingress = %q, want %q", got.Ingress, "domain:acme.example")
	}
	if !got.Verified {
		t.Error("YAML verified_domains hits must be Verified=true")
	}
}

func TestStrategyB_YAMLVerifiedDomain_StackOverride(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    verified_domains:
      acme.example:
        tenant: acme
        stack: acme/custom_mail_stack
`
	mr := mailResolverFor(t, yaml)
	got, ok := mr.ResolveRecipient("anyone@acme.example", "")
	if !ok {
		t.Fatal("expected hit")
	}
	if got.Stack != "acme/custom_mail_stack" {
		t.Errorf("stack = %q, want explicit override %q",
			got.Stack, "acme/custom_mail_stack")
	}
}

func TestStrategyB_YAMLVerifiedDomain_MissFallsThrough(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    verified_domains:
      acme.example:
        tenant: acme
    listeners:
      default:
        tenant: system
        stack: system/mail_drop
`
	mr := mailResolverFor(t, yaml)
	// Unverified domain — falls through to listener.
	got, ok := mr.ResolveRecipient("anyone@unknown.example", "default")
	if !ok {
		t.Fatal("expected listener fallback hit")
	}
	if got.Stack != "system/mail_drop" {
		t.Errorf("stack = %q, want listener fallback %q",
			got.Stack, "system/mail_drop")
	}
}

// TestStrategyB_YAML_SubdomainExactMatchOnly — verified_domains is
// exact-match per the ADR (§2.3). Verifying `app.acme.example`
// alone does NOT also route `support@acme.example`.
func TestStrategyB_YAML_SubdomainExactMatchOnly(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    verified_domains:
      app.acme.example:
        tenant: acme
`
	mr := mailResolverFor(t, yaml)
	if _, ok := mr.ResolveRecipient("anyone@app.acme.example", ""); !ok {
		t.Error("expected hit on exact subdomain")
	}
	// The apex (which is NOT in the map) must miss.
	if _, ok := mr.ResolveRecipient("anyone@acme.example", ""); ok {
		t.Error("apex unexpectedly matched (operator must verify apex explicitly)")
	}
}

// TestStrategyB_StrategyAWins — when Strategy A (`tenant.stack@host`)
// and YAML verified_domains could both fire for the same address,
// Strategy A wins by priority (rule 3 before rule 4).
func TestStrategyB_StrategyAWins(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    verified_domains:
      chassis.example:
        tenant: legacy
`
	r, err := LoadResolverFromFile(
		writeYAML(t, yaml),
		WithDefaultMailHosts([]string{"chassis.example"}),
	)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	mr := r.(MailResolver)
	got, ok := mr.ResolveRecipient("acme.support@chassis.example", "")
	if !ok {
		t.Fatal("expected hit")
	}
	// Strategy A parses tenant=acme, stack=support/_mail — must win
	// over verified_domains' tenant=legacy.
	if got.Tenant != "acme" || got.Stack != "support/_mail" {
		t.Errorf("Strategy A did not win: got tenant=%q stack=%q, want acme support/_mail",
			got.Tenant, got.Stack)
	}
}

// TestStrategyB_OperatorOverrideWins — explicit `recipients:` exact
// entry beats verified_domains even when both could fire.
func TestStrategyB_OperatorOverrideWins(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    recipients:
      "vip@acme.example":
        tenant: acme
        stack: acme/vip_inbox
    verified_domains:
      acme.example:
        tenant: acme
`
	mr := mailResolverFor(t, yaml)
	got, ok := mr.ResolveRecipient("vip@acme.example", "")
	if !ok {
		t.Fatal("expected hit")
	}
	if got.Stack != "acme/vip_inbox" {
		t.Errorf("override did not win: got %q, want %q", got.Stack, "acme/vip_inbox")
	}
	// Non-VIP rcpts on the same verified domain still hit Strategy B.
	got, ok = mr.ResolveRecipient("plebs@acme.example", "")
	if !ok {
		t.Fatal("expected verified_domains fallthrough hit")
	}
	if got.Stack != "_mail" {
		t.Errorf("got %q, want %q", got.Stack, "_mail")
	}
}

// TestStrategyB_ListenerStillReachable — verified_domains and
// listener can coexist; listener fires only when verified_domains
// misses.
func TestStrategyB_ListenerStillReachable(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    verified_domains:
      acme.example:
        tenant: acme
    listeners:
      default:
        tenant: system
        stack: system/mail_drop
`
	mr := mailResolverFor(t, yaml)

	got, _ := mr.ResolveRecipient("foo@acme.example", "default")
	if got.Stack != "_mail" {
		t.Errorf("verified_domains expected to win; got %q", got.Stack)
	}
	got, _ = mr.ResolveRecipient("foo@unrelated.example", "default")
	if got.Stack != "system/mail_drop" {
		t.Errorf("listener fallback expected; got %q", got.Stack)
	}
}

// ----- DB-backed (rule 4, tenant_hostnames) -----
//
// Verified-at writes happen inline via db.Exec because the seeded
// `newDBResolverTestStore` returns a raw *sql.DB; no need for a
// dedicated helper.

func TestStrategyB_DBVerifiedDomain_Hit(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostname(t, db, "thn_1", "acme.example", "tnt_a", "web")
	// Flip verified_at to "now".
	if _, err := db.Exec(
		`UPDATE tenant_hostnames SET verified_at = '2026-01-01T00:00:01Z' WHERE id = 'thn_1'`,
	); err != nil {
		t.Fatalf("verify: %v", err)
	}

	r := NewDBResolver(nil, db, nil, false)
	got, ok := r.ResolveRecipient("anyone@acme.example", "")
	if !ok {
		t.Fatalf("expected DB Strategy B hit")
	}
	if got.Tenant != "acme" {
		t.Errorf("tenant = %q, want acme", got.Tenant)
	}
	if got.Stack != "web/_mail" {
		t.Errorf("stack = %q, want web/_mail (nested under the bound stack h.stack=web)", got.Stack)
	}
	if !got.Verified {
		t.Error("verified row should produce Verified=true")
	}
	if got.Ingress != "domain:acme.example" {
		t.Errorf("ingress = %q, want %q", got.Ingress, "domain:acme.example")
	}
}

func TestStrategyB_DBVerifiedDomain_UnverifiedPermissive(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostname(t, db, "thn_1", "acme.example", "tnt_a", "")
	// verified_at left NULL (unverified)

	r := NewDBResolver(nil, db, nil, false) // permissive
	got, ok := r.ResolveRecipient("anyone@acme.example", "")
	if !ok {
		t.Fatal("permissive mode should route unverified rows (with a WARN)")
	}
	if got.Verified {
		t.Error("unverified row should have Verified=false")
	}
	if got.Stack != "_mail" {
		t.Errorf("stack = %q, want _mail", got.Stack)
	}
}

func TestStrategyB_DBVerifiedDomain_UnverifiedStrict(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostname(t, db, "thn_1", "acme.example", "tnt_a", "")
	// verified_at left NULL

	r := NewDBResolver(nil, db, nil, true) // strict
	if _, ok := r.ResolveRecipient("anyone@acme.example", ""); ok {
		t.Error("strict mode should miss on unverified rows")
	}
}

func TestStrategyB_DBVerifiedDomain_RevokedRowMisses(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostname(t, db, "thn_1", "acme.example", "tnt_a", "")
	if _, err := db.Exec(
		`UPDATE tenant_hostnames
		    SET verified_at = '2026-01-01T00:00:01Z',
		        revoked_at  = '2026-01-02T00:00:00Z'
		  WHERE id = 'thn_1'`,
	); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.ResolveRecipient("anyone@acme.example", ""); ok {
		t.Error("revoked row must not route")
	}
}

func TestStrategyB_DBVerifiedDomain_RevokedTenantMisses(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostname(t, db, "thn_1", "acme.example", "tnt_a", "")
	if _, err := db.Exec(
		`UPDATE tenant_hostnames SET verified_at = '2026-01-01T00:00:01Z' WHERE id = 'thn_1';
		 UPDATE tenants SET revoked_at = '2026-01-02T00:00:00Z' WHERE tenant_id = 'tnt_a';`,
	); err != nil {
		t.Fatalf("revoke tenant: %v", err)
	}

	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.ResolveRecipient("anyone@acme.example", ""); ok {
		t.Error("revoked tenant must not route")
	}
}

// TestStrategyB_DB_InnerYAMLOverridesWin — even with a DB hit,
// inner YAML's higher-priority rules (overrides, Strategy A, YAML
// verified_domains) must win.
func TestStrategyB_DB_InnerYAMLOverridesWin(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostname(t, db, "thn_1", "acme.example", "tnt_a", "")
	if _, err := db.Exec(
		`UPDATE tenant_hostnames SET verified_at = '2026-01-01T00:00:01Z' WHERE id = 'thn_1'`,
	); err != nil {
		t.Fatalf("verify: %v", err)
	}

	yamlR, err := LoadResolverFromFile(writeYAML(t, `
ingress:
  lmtp:
    recipients:
      "vip@acme.example":
        tenant: acme
        stack: acme/vip
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := NewDBResolver(yamlR, db, nil, false)

	got, _ := r.ResolveRecipient("vip@acme.example", "")
	if got.Stack != "acme/vip" {
		t.Errorf("inner YAML recipient override didn't win: got %q, want %q",
			got.Stack, "acme/vip")
	}
	// And a non-VIP rcpt falls through inner overrides → DB hit.
	got, _ = r.ResolveRecipient("intern@acme.example", "")
	if got.Stack != "_mail" {
		t.Errorf("DB Strategy B miss: got %q, want _mail", got.Stack)
	}
}

// TestStrategyB_DB_BeatsListener — the DB hit must fire BEFORE the
// listener catch-all (rule 4 < rule 5).
func TestStrategyB_DB_BeatsListener(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostname(t, db, "thn_1", "acme.example", "tnt_a", "")
	if _, err := db.Exec(
		`UPDATE tenant_hostnames SET verified_at = '2026-01-01T00:00:01Z' WHERE id = 'thn_1'`,
	); err != nil {
		t.Fatalf("verify: %v", err)
	}

	yamlR, err := LoadResolverFromFile(writeYAML(t, `
ingress:
  lmtp:
    listeners:
      default:
        tenant: system
        stack: system/mail_drop
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := NewDBResolver(yamlR, db, nil, false)

	got, ok := r.ResolveRecipient("intern@acme.example", "default")
	if !ok {
		t.Fatal("expected hit")
	}
	if got.Stack != "_mail" {
		t.Errorf("DB Strategy B should beat listener: got %q, want _mail (otherwise verified tenant's mail blackholes into operator catch-all)",
			got.Stack)
	}
}

// TestStrategyB_DB_NoMatchAllowsListenerFallback — when the DB has
// nothing for this domain AND inner YAML has nothing, the listener
// fallback still works.
func TestStrategyB_DB_NoMatchAllowsListenerFallback(t *testing.T) {
	db := newDBResolverTestStore(t)
	// No tenants seeded.

	yamlR, err := LoadResolverFromFile(writeYAML(t, `
ingress:
  lmtp:
    listeners:
      default:
        tenant: system
        stack: system/mail_drop
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	r := NewDBResolver(yamlR, db, nil, false)

	got, ok := r.ResolveRecipient("anyone@unknown.example", "default")
	if !ok {
		t.Fatal("expected listener fallback")
	}
	if got.Stack != "system/mail_drop" {
		t.Errorf("got %q, want listener fallback", got.Stack)
	}
}

// TestStrategyB_DB_LowercaseRcpt — rcpts are case-folded before DB
// lookup. `tenant_hostnames.hostname` is stored canonical (lower);
// the resolver lowercases incoming rcpts so an uppercase rcpt still
// matches a verified row.
func TestStrategyB_DB_LowercaseRcpt(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostname(t, db, "thn_1", "acme.example", "tnt_a", "")
	if _, err := db.Exec(
		`UPDATE tenant_hostnames SET verified_at = '2026-01-01T00:00:01Z' WHERE id = 'thn_1'`,
	); err != nil {
		t.Fatalf("verify: %v", err)
	}

	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.ResolveRecipient("Anyone@ACME.example", ""); !ok {
		t.Error("uppercase rcpt should hit after lowercase canonicalization")
	}
}
