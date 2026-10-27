package connection

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/loremlabs/thanks-computer/chassis/sandbox"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// Provider kinds: how authentication at the service works.
const (
	// KindOAuth2 is the authorization-code flow: the person signs in, the
	// chassis holds and renews the tokens.
	KindOAuth2 = "oauth2"
	// KindGitHubApp is OAuth 2 plus the app's own key, which mints
	// installation tokens (a later slice). Its user flow is oauth2's.
	KindGitHubApp = "github_app"
)

// Credential kinds: which credential a consumer uses, and what a row holds.
const (
	// CredentialOAuth2 is a user access token (and its refresh token).
	CredentialOAuth2 = "oauth2"
	// CredentialInstallation is a GitHub App installation token, minted
	// from the app's key (a later slice).
	CredentialInstallation = "github_app_installation"
)

// How the client authenticates at the token destination.
const (
	ClientAuthBody  = "body"
	ClientAuthBasic = "basic"
)

// Refresh-token rotation, the provider's own behaviour, declared rather than
// assumed.
const (
	// RotationSingleUse: a refresh token works once; the answer carries the
	// next one. Re-sending a used one is refused, and may end the family.
	RotationSingleUse = "single_use"
	// RotationReusable: the refresh token stays valid until revoked.
	RotationReusable = "reusable"
)

// What follows a renewal whose outcome is unknown (a timeout after the
// request was sent, a claim its holder never completed).
const (
	// OnUncertainReconnect: the old refresh token is never re-sent; the
	// current access token serves until it expires, then the owner
	// reconnects.
	OnUncertainReconnect = "reconnect"
	// OnUncertainRetry: one more try, under a new claim.
	OnUncertainRetry = "retry"
)

// Ceilings on one declaration.
const (
	MaxTitle     = 64
	MaxScopes    = 32
	MaxOrigins   = 16
	MaxConsumers = 64
)

// Margin defaults: renewal starts when a fifth of the lifetime is left,
// never under a minute before expiry, never more than 15 minutes before —
// and never in a token's first half-life (Provider.Margin).
const (
	DefaultMarginFraction = 0.2
	DefaultMarginFloorS   = 60
	DefaultMarginCeilingS = 900
)

// ErrNotDeclared: no active stack of the tenant declares the provider, or
// the stack has no consumer declaration for it.
var ErrNotDeclared = errors.New("connection: not declared")

// Destinations is where each credential may go. Each kind of credential
// reaches one kind of destination and no other (Service.send).
type Destinations struct {
	// Authorization is where the browser goes to sign in. No credential is
	// sent there.
	Authorization string `yaml:"authorization"`
	// Token is where a code or refresh token is exchanged: the client
	// credentials go here and nowhere else.
	Token string `yaml:"token"`
	// Revocation is "none", or an RFC 7009 endpoint the token being revoked
	// is posted to.
	Revocation string `yaml:"revocation"`
	// API is every origin an access token may be sent to; a consumer
	// narrows it to the origins it calls.
	API []string `yaml:"api"`
}

// Account says how to learn who signed in: a GET at URL (an api origin)
// with the new token; Subject is the field holding the provider's stable id,
// Name the field holding the display name.
type Account struct {
	URL     string `yaml:"url"`
	Subject string `yaml:"subject"`
	Name    string `yaml:"name"`
}

// Margin is when renewal starts: a fraction of the lifetime, clamped.
type Margin struct {
	Fraction float64 `yaml:"fraction"`
	FloorS   int     `yaml:"floor_s"`
	CeilingS int     `yaml:"ceiling_s"`
}

// Refresh is the provider's renewal behaviour.
type Refresh struct {
	Rotation    string `yaml:"rotation"`
	OnUncertain string `yaml:"on_uncertain"`
	Margin      Margin `yaml:"margin"`
}

// App is a GitHub App's own credentials (kind github_app).
type App struct {
	ID         string `yaml:"id"`
	PrivateKey string `yaml:"private_key"`
}

// Provider is one PROVIDERS/<name>.yaml: how authentication at a service
// works, where each credential may go, and which stacks may use its
// connections for which capabilities. Parsed strictly: an unknown key is a
// deploy error.
type Provider struct {
	Title        string              `yaml:"title"`
	Kind         string              `yaml:"kind"`
	Issuer       string              `yaml:"issuer"`
	Destinations Destinations        `yaml:"destinations"`
	ClientID     string              `yaml:"client_id"`
	ClientSecret string              `yaml:"client_secret"`
	ClientAuth   string              `yaml:"client_auth"`
	PKCE         bool                `yaml:"pkce"`
	Scopes       []string            `yaml:"scopes"`
	Account      *Account            `yaml:"account"`
	Refresh      Refresh             `yaml:"refresh"`
	App          *App                `yaml:"app"`
	Consumers    map[string][]string `yaml:"consumers"`

	api originSet // parsed Destinations.API
}

// Consumer is one CONNECTIONS/<provider>.yaml: how the declaring stack uses
// an identity at that provider.
type Consumer struct {
	Provider string   `yaml:"provider"`
	Kind     string   `yaml:"kind"`
	Origins  []string `yaml:"origins"`
	Use      Use      `yaml:"use"`

	origins originSet
}

// Use is how the token goes on a request: a header and a `{}` template.
type Use struct {
	Header string `yaml:"header"`
	Format string `yaml:"format"`
}

var (
	headerRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
	stackRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	// secretRefRe mirrors the secret store's name rule (chassis/secrets).
	secretRefRe = regexp.MustCompile(`^secret:[A-Za-z][A-Za-z0-9_]{0,127}$`)
)

// ParseProvider strictly decodes and validates a provider declaration.
func ParseProvider(data []byte) (*Provider, error) {
	var p Provider
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("provider declaration: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("provider declaration: %w", err)
	}
	return &p, nil
}

func (p *Provider) validate() error {
	p.Title = strings.TrimSpace(p.Title)
	if len(p.Title) > MaxTitle {
		return fmt.Errorf("title is longer than %d characters", MaxTitle)
	}
	switch p.Kind {
	case KindOAuth2, KindGitHubApp:
	case "":
		return fmt.Errorf("kind is required (%s or %s)", KindOAuth2, KindGitHubApp)
	default:
		return fmt.Errorf("kind %q: want %s or %s", p.Kind, KindOAuth2, KindGitHubApp)
	}
	if p.Issuer != "" {
		if _, err := ParseOrigin(p.Issuer); err != nil {
			return fmt.Errorf("issuer: %v", err)
		}
	}
	for field, raw := range map[string]string{"authorization": p.Destinations.Authorization, "token": p.Destinations.Token} {
		if raw == "" {
			return fmt.Errorf("destinations.%s is required", field)
		}
		if err := checkEndpoint(raw); err != nil {
			return fmt.Errorf("destinations.%s: %v", field, err)
		}
	}
	switch p.Destinations.Revocation {
	case "", "none":
		p.Destinations.Revocation = "none"
	default:
		if err := checkEndpoint(p.Destinations.Revocation); err != nil {
			return fmt.Errorf("destinations.revocation: %v (or none)", err)
		}
	}
	if len(p.Destinations.API) > MaxOrigins {
		return fmt.Errorf("destinations.api names %d origins, at most %d", len(p.Destinations.API), MaxOrigins)
	}
	api, _, err := parseOrigins("destinations.api", p.Destinations.API)
	if err != nil {
		return err
	}
	p.api = api
	for field, ref := range map[string]string{"client_id": p.ClientID, "client_secret": p.ClientSecret} {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("%s is required: a literal, or secret:NAME", field)
		}
		if strings.HasPrefix(ref, "secret:") && !secretRefRe.MatchString(ref) {
			return fmt.Errorf("%s: a secret reference is secret:NAME (a letter, then letters, digits and _)", field)
		}
	}
	if !strings.HasPrefix(p.ClientSecret, "secret:") {
		return fmt.Errorf("client_secret must be a secret reference (secret:NAME), never a literal")
	}
	switch p.ClientAuth {
	case "":
		p.ClientAuth = ClientAuthBody
	case ClientAuthBody, ClientAuthBasic:
	default:
		return fmt.Errorf("client_auth %q: want %s or %s", p.ClientAuth, ClientAuthBody, ClientAuthBasic)
	}
	if len(p.Scopes) > MaxScopes {
		return fmt.Errorf("scopes names %d, at most %d", len(p.Scopes), MaxScopes)
	}
	for i, s := range p.Scopes {
		s = strings.TrimSpace(s)
		if s == "" || strings.ContainsAny(s, " \t\r\n\"") {
			return fmt.Errorf("scopes[%d]: %q is not a scope", i, s)
		}
		p.Scopes[i] = s
	}
	if p.Account != nil {
		if p.Account.URL == "" || p.Account.Subject == "" || p.Account.Name == "" {
			return fmt.Errorf("account: url, subject and name are all required")
		}
		u, err := url.Parse(p.Account.URL)
		if err != nil {
			return fmt.Errorf("account.url: %v", err)
		}
		o, err := OriginOf(u)
		if err != nil {
			return fmt.Errorf("account.url: %v", err)
		}
		if !p.api.has(o) {
			return fmt.Errorf("account.url: %s is not one of destinations.api", o)
		}
	}
	switch p.Refresh.Rotation {
	case "":
		p.Refresh.Rotation = RotationSingleUse
	case RotationSingleUse, RotationReusable:
	default:
		return fmt.Errorf("refresh.rotation %q: want %s or %s", p.Refresh.Rotation, RotationSingleUse, RotationReusable)
	}
	switch p.Refresh.OnUncertain {
	case "":
		p.Refresh.OnUncertain = OnUncertainReconnect
	case OnUncertainReconnect, OnUncertainRetry:
	default:
		return fmt.Errorf("refresh.on_uncertain %q: want %s or %s", p.Refresh.OnUncertain, OnUncertainReconnect, OnUncertainRetry)
	}
	m := &p.Refresh.Margin
	if m.Fraction == 0 {
		m.Fraction = DefaultMarginFraction
	}
	if m.FloorS == 0 {
		m.FloorS = DefaultMarginFloorS
	}
	if m.CeilingS == 0 {
		m.CeilingS = DefaultMarginCeilingS
	}
	if m.Fraction <= 0 || m.Fraction > 0.5 {
		return fmt.Errorf("refresh.margin.fraction %v: want more than 0 and at most 0.5", m.Fraction)
	}
	if m.FloorS < 0 || m.CeilingS < m.FloorS {
		return fmt.Errorf("refresh.margin: floor_s %d and ceiling_s %d: want 0 <= floor_s <= ceiling_s", m.FloorS, m.CeilingS)
	}
	if p.Kind == KindGitHubApp {
		if p.App == nil || p.App.ID == "" || p.App.PrivateKey == "" {
			return fmt.Errorf("app.id and app.private_key are required for kind %s", KindGitHubApp)
		}
		if !secretRefRe.MatchString(p.App.PrivateKey) {
			return fmt.Errorf("app.private_key must be a secret reference (secret:NAME)")
		}
		if strings.HasPrefix(p.App.ID, "secret:") && !secretRefRe.MatchString(p.App.ID) {
			return fmt.Errorf("app.id: a secret reference is secret:NAME")
		}
	} else if p.App != nil {
		return fmt.Errorf("app is for kind %s only", KindGitHubApp)
	}
	if len(p.Consumers) > MaxConsumers {
		return fmt.Errorf("consumers names %d stacks, at most %d", len(p.Consumers), MaxConsumers)
	}
	for stack, caps := range p.Consumers {
		if !stackRe.MatchString(stack) {
			return fmt.Errorf("consumers.%s: not a stack name", stack)
		}
		if len(caps) == 0 {
			return fmt.Errorf("consumers.%s: names no capability (a list of the capabilities this stack may use the connection for)", stack)
		}
		seen := map[string]bool{}
		for i, c := range caps {
			c = strings.TrimSpace(c)
			if !sandbox.ValidCapability(c) {
				return fmt.Errorf("consumers.%s[%d]: %q is not a capability name", stack, i, c)
			}
			if seen[c] {
				return fmt.Errorf("consumers.%s names %q twice", stack, c)
			}
			seen[c] = true
			caps[i] = c
		}
	}
	return nil
}

// checkEndpoint accepts an https URL with a path (http to loopback), no
// user info, no fragment.
func checkEndpoint(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if _, err := OriginOf(u); err != nil {
		return err
	}
	if u.Scheme == "http" && !Loopback(strings.ToLower(u.Hostname())) {
		return fmt.Errorf("%q: http:// is allowed only to localhost", raw)
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%q: no user info or fragment", raw)
	}
	return nil
}

// Allows reports whether stack may use this provider's connections for the
// capability. No wildcards: the stack and the capability are both named.
func (p *Provider) Allows(stack, capability string) bool {
	for _, c := range p.Consumers[stack] {
		if c == capability {
			return true
		}
	}
	return false
}

// APIOrigin reports whether o is one of the provider's api destinations.
func (p *Provider) APIOrigin(o Origin) bool { return p.api.has(o) }

// ConsumerStacks is the sorted list of stacks the provider names.
func (p *Provider) ConsumerStacks() []string {
	out := make([]string, 0, len(p.Consumers))
	for s := range p.Consumers {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// SecretName returns the tenant secret a `secret:NAME` reference names, or
// ("", false) for a literal.
func SecretName(ref string) (string, bool) {
	if strings.HasPrefix(ref, "secret:") {
		return strings.TrimPrefix(ref, "secret:"), true
	}
	return "", false
}

// ParseConsumer strictly decodes and validates a consumer declaration. The
// file's own name (the provider it is for) is checked by the caller against
// Consumer.Provider.
func ParseConsumer(data []byte) (*Consumer, error) {
	var c Consumer
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("connection declaration: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("connection declaration: %w", err)
	}
	return &c, nil
}

func (c *Consumer) validate() error {
	c.Provider = strings.TrimSpace(c.Provider)
	if !ValidName(c.Provider) {
		return fmt.Errorf("provider %q: want the provider's name (a PROVIDERS/<name>.yaml of an active stack)", c.Provider)
	}
	switch c.Kind {
	case "":
		c.Kind = CredentialOAuth2
	case CredentialOAuth2, CredentialInstallation:
	default:
		return fmt.Errorf("kind %q: want %s or %s", c.Kind, CredentialOAuth2, CredentialInstallation)
	}
	if len(c.Origins) > MaxOrigins {
		return fmt.Errorf("origins names %d, at most %d", len(c.Origins), MaxOrigins)
	}
	set, _, err := parseOrigins("origins", c.Origins)
	if err != nil {
		return err
	}
	c.origins = set
	if c.Use.Header == "" {
		c.Use.Header = "authorization"
	}
	if !headerRe.MatchString(c.Use.Header) {
		return fmt.Errorf("use.header %q: not a header name", c.Use.Header)
	}
	c.Use.Header = strings.ToLower(c.Use.Header)
	if c.Use.Format == "" {
		c.Use.Format = "Bearer {}"
	}
	if err := secrets.ValidateFormat(c.Use.Format); err != nil {
		return fmt.Errorf("use.format: %v", err)
	}
	return nil
}

// OriginAllowed reports whether the consumer declares o.
func (c *Consumer) OriginAllowed(o Origin) bool { return c.origins.has(o) }

// OriginList is the consumer's origins, parsed, in declaration order.
func (c *Consumer) OriginList() []Origin {
	_, list, _ := parseOrigins("origins", c.Origins)
	return list
}
