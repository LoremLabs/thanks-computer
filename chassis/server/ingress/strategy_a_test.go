package ingress

import (
	"testing"
)

// strategyAResolver returns a MailResolver with only Strategy A
// hosts configured — no operator-override maps, no listener
// fallback. Exercises Strategy A in isolation.
func strategyAResolver(t *testing.T, hosts []string) MailResolver {
	t.Helper()
	r, err := LoadResolverFromFile("", WithDefaultMailHosts(hosts))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil resolver (Strategy A only)")
	}
	mr, ok := r.(MailResolver)
	if !ok {
		t.Fatal("resolver does not satisfy MailResolver")
	}
	return mr
}

func TestStrategyA_BasicParse(t *testing.T) {
	mr := strategyAResolver(t, []string{"chassis.example"})
	got, ok := mr.ResolveRecipient("acme.support@chassis.example", "default")
	if !ok {
		t.Fatalf("expected Strategy A hit")
	}
	if got.Tenant != "acme" {
		t.Errorf("tenant = %q, want %q", got.Tenant, "acme")
	}
	if got.Stack != "acme/support" {
		t.Errorf("stack = %q, want %q", got.Stack, "acme/support")
	}
	if !got.Verified {
		t.Error("Strategy A hits must be Verified=true")
	}
}

// TestStrategyA_ModifierDoesNotAffectRouting — `+monday` and `+tuesday`
// route to the same (tenant, stack). The modifier is parsed off but
// NOT carried on RouteTarget: rules that want it read `_txc.lmtp.rcpt[i]`
// and split on '+' (the source of truth).
func TestStrategyA_ModifierDoesNotAffectRouting(t *testing.T) {
	mr := strategyAResolver(t, []string{"chassis.example"})
	a, ok := mr.ResolveRecipient("acme.support+monday@chassis.example", "")
	if !ok {
		t.Fatal("expected hit with modifier")
	}
	b, ok := mr.ResolveRecipient("acme.support+tuesday@chassis.example", "")
	if !ok {
		t.Fatal("expected hit with modifier")
	}
	if a.Tenant != b.Tenant || a.Stack != b.Stack {
		t.Errorf("same tenant.stack must route to same target regardless of modifier: a=%+v b=%+v", a, b)
	}
}

// TestStrategyA_MultipleHosts — operators with several MX-receiving
// names can list them all; rcpts on any host match.
func TestStrategyA_MultipleHosts(t *testing.T) {
	mr := strategyAResolver(t, []string{"chassis.example", "mail.chassis.example"})

	if _, ok := mr.ResolveRecipient("acme.support@chassis.example", ""); !ok {
		t.Error("expected hit on chassis.example")
	}
	if _, ok := mr.ResolveRecipient("acme.support@mail.chassis.example", ""); !ok {
		t.Error("expected hit on mail.chassis.example")
	}
	// Host case-folded — RFC 5321 §2.3.11.
	if _, ok := mr.ResolveRecipient("ACME.SUPPORT@CHASSIS.EXAMPLE", ""); !ok {
		t.Error("expected hit on uppercase rcpt (rcpt is lowercased before parse)")
	}
}

// TestStrategyA_UnknownHostFallsThrough — Strategy A doesn't fire on
// hosts not in defaultMailHosts. Missing both Strategy A and
// operator overrides → returns ok=false.
func TestStrategyA_UnknownHostFallsThrough(t *testing.T) {
	mr := strategyAResolver(t, []string{"chassis.example"})
	if _, ok := mr.ResolveRecipient("acme.support@elsewhere.example", ""); ok {
		t.Error("expected miss on unknown host")
	}
}

// TestStrategyA_InvalidSlugFallsThrough — local-parts that don't fit
// the slug shape don't match Strategy A. The address falls through;
// since no other rule matches in this resolver, the test returns ok=false.
func TestStrategyA_InvalidSlugFallsThrough(t *testing.T) {
	mr := strategyAResolver(t, []string{"chassis.example"})
	cases := []struct {
		name string
		rcpt string
	}{
		{"no_dot", "acme@chassis.example"},
		{"empty_tenant", ".stack@chassis.example"},
		{"empty_stack", "acme.@chassis.example"},
		{"tenant_starts_with_digit", "1acme.stack@chassis.example"},
		{"tenant_contains_dot", "acme.foo.stack@chassis.example"}, // first-dot split gives stack="foo.stack" → invalid
		{"uppercase_after_lowercase", "Acme.Stack@chassis.example"}, // lowercased to "acme.stack" → actually valid; not a fall-through case
		{"tenant_with_underscore", "ac_me.stack@chassis.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := mr.ResolveRecipient(tc.rcpt, "")
			// The uppercase case lowercases to valid; explicitly skip it.
			if tc.name == "uppercase_after_lowercase" {
				if !ok {
					t.Errorf("uppercase rcpt should hit after lowercasing")
				}
				return
			}
			if ok {
				t.Errorf("%q unexpectedly matched Strategy A", tc.rcpt)
			}
		})
	}
}

// TestStrategyA_OverrideWins — when an operator has an exact
// `recipients:` entry for an address that ALSO matches Strategy A,
// the override wins. Tests the priority order locked in
// internal docs/todo-lmtp-routing-v2.md §2.5.
func TestStrategyA_OverrideWins(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    recipients:
      "acme.support@chassis.example":
        tenant: acme
        stack: acme/override_stack
`
	r, err := LoadResolverFromFile(writeYAML(t, yaml), WithDefaultMailHosts([]string{"chassis.example"}))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	mr := r.(MailResolver)
	got, ok := mr.ResolveRecipient("acme.support@chassis.example", "")
	if !ok {
		t.Fatalf("expected hit")
	}
	if got.Stack != "acme/override_stack" {
		t.Errorf("override did not win: got %q, want %q (Strategy A would route to acme/support)",
			got.Stack, "acme/override_stack")
	}
}

// TestStrategyA_DisabledWhenHostsEmpty — without configured hosts,
// the parser is dormant; even a Strategy-A-shaped address falls
// through to other rules.
func TestStrategyA_DisabledWhenHostsEmpty(t *testing.T) {
	yaml := `
ingress:
  lmtp:
    listeners:
      default:
        tenant: system
        stack: system/drop
`
	r, err := LoadResolverFromFile(writeYAML(t, yaml)) // no default hosts
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	mr := r.(MailResolver)
	got, ok := mr.ResolveRecipient("acme.support@chassis.example", "default")
	if !ok {
		t.Fatalf("expected hit on listener fallback")
	}
	if got.Stack != "system/drop" {
		t.Errorf("Strategy A unexpectedly fired with empty hosts; got %q", got.Stack)
	}
}

// TestStrategyA_NoFileNoHosts — the truly degenerate case: no YAML
// file AND no default hosts. LoadResolverFromFile returns nil.
func TestStrategyA_NoFileNoHosts(t *testing.T) {
	r, err := LoadResolverFromFile("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r != nil {
		t.Error("expected nil resolver when nothing is configured")
	}
}

// TestStrategyA_FileLessButHostsConfigured — embedders running with
// Strategy A only (no YAML overrides) still get a working resolver.
func TestStrategyA_FileLessButHostsConfigured(t *testing.T) {
	r, err := LoadResolverFromFile("", WithDefaultMailHosts([]string{"chassis.example"}))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r == nil {
		t.Fatal("expected resolver — Strategy A is configured even without a YAML file")
	}
	mr := r.(MailResolver)
	if _, ok := mr.ResolveRecipient("acme.support@chassis.example", ""); !ok {
		t.Error("Strategy A failed without a YAML file")
	}
}

// Direct unit tests for the parser — exhaustive corner cases.
func TestParseStrategyALocal(t *testing.T) {
	cases := []struct {
		in       string
		wantOk   bool
		tenant   string
		stack    string
		modifier string
	}{
		{"acme.support", true, "acme", "support", ""},
		{"acme.support+monday", true, "acme", "support", "monday"},
		{"a-b.c-d+x.y.z", true, "a-b", "c-d", "x.y.z"}, // modifier is opaque, can have dots
		{"acme.support+", true, "acme", "support", ""}, // empty modifier ok
		{"a.b", true, "a", "b", ""},
		{"a1.b2", true, "a1", "b2", ""},

		// Fall-throughs
		{"", false, "", "", ""},
		{".support", false, "", "", ""},
		{"acme.", false, "", "", ""},
		{"acme", false, "", "", ""},
		{"acme.foo.bar", false, "", "", ""}, // stack="foo.bar" invalid
		{"1acme.support", false, "", "", ""},
		{"acme.1support", false, "", "", ""},
		{"acme.sup_port", false, "", "", ""},
		{"-acme.support", false, "", "", ""},
		{"acme.-support", false, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			tenant, stack, modifier, ok := parseStrategyALocal(tc.in)
			if ok != tc.wantOk {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOk)
			}
			if !ok {
				return
			}
			if tenant != tc.tenant || stack != tc.stack || modifier != tc.modifier {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					tenant, stack, modifier, tc.tenant, tc.stack, tc.modifier)
			}
		})
	}
}

func TestIsSlug(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"acme", true},
		{"a", true},
		{"a-b-c", true},
		{"a1b2", true},
		{"acme-co", true},
		{"", false},
		{"-acme", false},
		{"1acme", false},
		{"acme_co", false},
		{"acme.co", false},
		{"Acme", false}, // uppercase rejected; caller lowercases before parse
		{"acme co", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := isSlug(tc.in); got != tc.want {
				t.Errorf("isSlug(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
