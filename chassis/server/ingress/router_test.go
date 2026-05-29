package ingress

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"
)

const sampleYAML = `
ingress:
  http:
    hosts:
      tenant1.example.com:
        tenant: tenant1
        stack: tenant1/web
      acme.local:
        tenant: acme
        stack: acme/web
  tcp:
    listeners:
      smtp-in:
        tenant: tenant1
        stack: tenant1/mail
  cron:
    jobs:
      nightly-reconcile:
        tenant: system
        stack: system/cron
  lmtp:
    listeners:
      default:
        tenant: system
        stack: system/mail_catchall
    recipients:
      "support@acme.example":
        tenant: acme
        stack: acme/support
      "@beta.example":
        tenant: beta
        stack: beta/catchall
`

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "ingress.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

func TestResolverMatchesHTTPHost(t *testing.T) {
	r, err := LoadResolverFromFile(writeYAML(t, sampleYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := r.Resolve(RouteKey{Src: "http", Hostname: "tenant1.example.com"})
	if !ok {
		t.Fatalf("expected hit on tenant1.example.com")
	}
	if got.Tenant != "tenant1" || got.Stack != "tenant1/web" || got.Ingress != "tenant1.example.com" {
		t.Errorf("got %+v, want {tenant1, tenant1/web, tenant1.example.com}", got)
	}
}

func TestResolverMissesUnknownHost(t *testing.T) {
	r, err := LoadResolverFromFile(writeYAML(t, sampleYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "unknown.host"}); ok {
		t.Error("expected miss for unknown.host")
	}
}

func TestResolverMatchesTCPListener(t *testing.T) {
	r, err := LoadResolverFromFile(writeYAML(t, sampleYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "smtp-in"})
	if !ok {
		t.Fatalf("expected hit on smtp-in")
	}
	if got.Tenant != "tenant1" || got.Stack != "tenant1/mail" || got.Ingress != "smtp-in" {
		t.Errorf("got %+v, want {tenant1, tenant1/mail, smtp-in}", got)
	}
}

func TestResolverMatchesCronJob(t *testing.T) {
	r, err := LoadResolverFromFile(writeYAML(t, sampleYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got, ok := r.Resolve(RouteKey{Src: "cron", Job: "nightly-reconcile"})
	if !ok {
		t.Fatalf("expected hit on nightly-reconcile")
	}
	if got.Tenant != "system" || got.Stack != "system/cron" || got.Ingress != "nightly-reconcile" {
		t.Errorf("got %+v, want {system, system/cron, nightly-reconcile}", got)
	}
}

// TestResolverIsolatesSources locks in that a hostname matching the http
// block does NOT also resolve when Src is "tcp" — sources are siloed and
// must not bleed into each other.
func TestResolverIsolatesSources(t *testing.T) {
	r, err := LoadResolverFromFile(writeYAML(t, sampleYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// http hostname presented as tcp Listener: should miss.
	if _, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "tenant1.example.com"}); ok {
		t.Error("hostname leaked into tcp source space")
	}
	// tcp listener name presented as http Hostname: should miss.
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "smtp-in"}); ok {
		t.Error("listener name leaked into http source space")
	}
	// LMTP routing isn't on Resolve(RouteKey) anymore — it's on
	// MailResolver.ResolveRecipient. So an lmtp recipient string
	// presented as an http Hostname must miss (different keyspaces).
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "support@acme.example"}); ok {
		t.Error("lmtp recipient leaked into http source space")
	}
}

// ResolveRecipient lives on the MailResolver add-on interface; the
// concrete *yamlResolver returned by LoadResolverFromFile satisfies
// it. Tests type-assert through MailResolver to make the contract
// explicit.
func mailResolverFor(t *testing.T, yaml string) MailResolver {
	t.Helper()
	r, err := LoadResolverFromFile(writeYAML(t, yaml))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	mr, ok := r.(MailResolver)
	if !ok {
		t.Fatalf("resolver does not satisfy MailResolver")
	}
	return mr
}

// TestResolverMatchesLMTPRecipientExact — exact-address recipient
// match takes priority over any @domain wildcard.
func TestResolverMatchesLMTPRecipientExact(t *testing.T) {
	mr := mailResolverFor(t, sampleYAML)
	got, ok := mr.ResolveRecipient("support@acme.example", "default")
	if !ok {
		t.Fatalf("expected hit on support@acme.example")
	}
	if got.Tenant != "acme" || got.Stack != "acme/support" || got.Ingress != "support@acme.example" {
		t.Errorf("got %+v, want {acme, acme/support, support@acme.example}", got)
	}
	if !got.Verified {
		t.Error("YAML hits must be Verified=true")
	}
}

// TestResolverMatchesLMTPDomainWildcard — recipients keyed as
// "@<domain>" catch any unmatched address for that domain.
func TestResolverMatchesLMTPDomainWildcard(t *testing.T) {
	mr := mailResolverFor(t, sampleYAML)
	got, ok := mr.ResolveRecipient("anyone@beta.example", "default")
	if !ok {
		t.Fatalf("expected wildcard hit for anyone@beta.example")
	}
	if got.Tenant != "beta" || got.Stack != "beta/catchall" || got.Ingress != "@beta.example" {
		t.Errorf("got %+v, want {beta, beta/catchall, @beta.example}", got)
	}
}

// TestResolverMatchesLMTPListenerFallback — neither the exact
// recipient nor a @domain entry matches; routing falls through to the
// listener catch-all so unrouted mail can still land in a
// `system/mail_catchall` stack that decides what to do with it.
func TestResolverMatchesLMTPListenerFallback(t *testing.T) {
	mr := mailResolverFor(t, sampleYAML)
	got, ok := mr.ResolveRecipient("nobody@unknown.example", "default")
	if !ok {
		t.Fatalf("expected listener fallback hit")
	}
	if got.Tenant != "system" || got.Stack != "system/mail_catchall" || got.Ingress != "default" {
		t.Errorf("got %+v, want {system, system/mail_catchall, default}", got)
	}
}

// TestResolverMissesLMTPWithoutFallback — listener that isn't
// configured + recipient that isn't configured returns ok=false. The
// LMTP inlet then default-denies (550) for that recipient.
func TestResolverMissesLMTPWithoutFallback(t *testing.T) {
	mr := mailResolverFor(t, sampleYAML)
	if _, ok := mr.ResolveRecipient("x@y.example", "unconfigured"); ok {
		t.Error("expected miss on unconfigured listener")
	}
	// And the empty-key edge case — no recipient, no listener — must
	// not panic on the strings.LastIndex("@") call.
	if _, ok := mr.ResolveRecipient("", ""); ok {
		t.Error("expected miss on empty lmtp key")
	}
}

// TestResolverLMTPExactBeforeWildcard — priority order matters: if
// both `support@acme.example` and `@acme.example` are present, the
// exact entry must win.
func TestResolverLMTPExactBeforeWildcard(t *testing.T) {
	mr := mailResolverFor(t, `
ingress:
  lmtp:
    recipients:
      "support@acme.example":
        tenant: acme
        stack: acme/support
      "@acme.example":
        tenant: acme
        stack: acme/catchall
`)
	got, ok := mr.ResolveRecipient("support@acme.example", "")
	if !ok {
		t.Fatalf("expected hit")
	}
	if got.Stack != "acme/support" {
		t.Errorf("exact match did not win: got %q, want %q (wildcard would steal traffic)",
			got.Stack, "acme/support")
	}
	got, ok = mr.ResolveRecipient("noreply@acme.example", "")
	if !ok {
		t.Fatalf("expected wildcard hit for noreply@")
	}
	if got.Stack != "acme/catchall" {
		t.Errorf("wildcard miss: got %q, want %q", got.Stack, "acme/catchall")
	}
}

// TestResolverLMTPLowercasing — case-folds rcpt before matching so
// "Support@Acme.Example" and "support@acme.example" route the same.
// RFC 5321 §2.3.11 says the domain is case-insensitive; conventionally
// the local-part is too.
func TestResolverLMTPLowercasing(t *testing.T) {
	mr := mailResolverFor(t, sampleYAML)
	got, ok := mr.ResolveRecipient("SUPPORT@ACME.EXAMPLE", "default")
	if !ok {
		t.Fatalf("expected hit on uppercase rcpt")
	}
	if got.Stack != "acme/support" {
		t.Errorf("got %q, want %q", got.Stack, "acme/support")
	}
}

func TestLoadResolverFromFileEmptyPath(t *testing.T) {
	r, err := LoadResolverFromFile("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r != nil {
		t.Error("expected nil resolver for empty path (no ingress configured)")
	}
}

func TestLoadResolverFromFileMissingFile(t *testing.T) {
	_, err := LoadResolverFromFile("/no/such/path/ingress.yaml")
	if err == nil {
		t.Error("expected error for missing file (fail-fast at startup)")
	}
}

func TestLoadResolverFromFileBadYAML(t *testing.T) {
	_, err := LoadResolverFromFile(writeYAML(t, "ingress:\n  http: [this is not a map"))
	if err == nil {
		t.Error("expected error for malformed YAML")
	}
}

func TestKeyFromEnvelopeReadsAllSignals(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want RouteKey
	}{
		{
			name: "http",
			raw:  `{"_txc":{"src":"http","web":{"req":{"host":"acme.local","url":{"path":"/api/v1/x"}}}}}`,
			want: RouteKey{Src: "http", Hostname: "acme.local", Path: "/api/v1/x"},
		},
		{
			name: "tcp",
			raw:  `{"_txc":{"src":"tcp","tcp":{"listener":"smtp-in"}}}`,
			want: RouteKey{Src: "tcp", Listener: "smtp-in"},
		},
		{
			name: "cron",
			raw:  `{"_txc":{"src":"cron","cron":{"job":"nightly-reconcile"}}}`,
			want: RouteKey{Src: "cron", Job: "nightly-reconcile"},
		},
		{
			name: "lmtp (KeyFromEnvelope does not read lmtp fields; LMTP routes via MailResolver, not Resolve)",
			raw:  `{"_txc":{"src":"lmtp","lmtp":{"listener":"default","rcpt":["a@b"]}}}`,
			want: RouteKey{Src: "lmtp"},
		},
		{
			name: "empty envelope",
			raw:  `{}`,
			want: RouteKey{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := KeyFromEnvelope(c.raw)
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestStampEnvelopeWritesAllFields(t *testing.T) {
	in := `{"x":1,"_txc":{"src":"http","rid":"r1"}}`
	out := StampEnvelope(in, RouteTarget{Tenant: "t1", Stack: "t1/web", Ingress: "t1.example.com"})

	if got := gjson.Get(out, "_txc.tenant").String(); got != "t1" {
		t.Errorf("_txc.tenant = %q, want %q", got, "t1")
	}
	if got := gjson.Get(out, "_txc.stack").String(); got != "t1/web" {
		t.Errorf("_txc.stack = %q, want %q", got, "t1/web")
	}
	if got := gjson.Get(out, "_txc.ingress").String(); got != "t1.example.com" {
		t.Errorf("_txc.ingress = %q, want %q", got, "t1.example.com")
	}
	// preserves existing fields
	if got := gjson.Get(out, "x").Int(); got != 1 {
		t.Errorf("preserved field x = %d, want 1", got)
	}
	if got := gjson.Get(out, "_txc.src").String(); got != "http" {
		t.Errorf("preserved field _txc.src = %q, want %q", got, "http")
	}
	if got := gjson.Get(out, "_txc.rid").String(); got != "r1" {
		t.Errorf("preserved field _txc.rid = %q, want %q", got, "r1")
	}
}

func TestStampEnvelopeOnEmptyString(t *testing.T) {
	out := StampEnvelope("", RouteTarget{Tenant: "t1", Stack: "t1/web", Ingress: "x"})
	if got := gjson.Get(out, "_txc.tenant").String(); got != "t1" {
		t.Errorf("empty-input case: _txc.tenant = %q, want %q", got, "t1")
	}
}

// TestStampEnvelopeHostnameVerified — _txc.hostname_verified mirrors
// RouteTarget.Verified verbatim. Stack rules read this to gate on
// proven-ownership routing without re-querying the DB.
func TestStampEnvelopeHostnameVerified(t *testing.T) {
	for _, tc := range []struct {
		name     string
		verified bool
	}{
		{"verified", true},
		{"unverified", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := StampEnvelope(`{}`,
				RouteTarget{Tenant: "t1", Stack: "t1/web", Ingress: "host:t1.example.com", Verified: tc.verified})
			got := gjson.Get(out, "_txc.hostname_verified").Bool()
			if got != tc.verified {
				t.Errorf("_txc.hostname_verified = %v, want %v", got, tc.verified)
			}
		})
	}
}
