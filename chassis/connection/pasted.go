package connection

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// A pasted key has to reach the store without entering an envelope. A trace
// records a request's body from its first record. A redact hint starts only
// when the request reaches the stack that declares it, and every request
// starts in `_sys`, so the inbound record and the boot steps are written with
// the body in them. Scrubbing by value starts later still, at the op that sees
// the key.
//
// So a request sent with PastedKeyHeader has its body kept off the envelope.
// The web head puts it on the request's Go context, where no rule can reach
// it, and txco://connection/put takes it from there, once. The header is set
// by whoever sends the request and grants nothing: it only keeps a body away
// from the rules that would otherwise have read it.

// PastedKeyHeader marks a request whose body is a key being pasted.
const PastedKeyHeader = "X-Txco-Pasted-Key"

// MaxKeyBytes bounds a pasted key. Real keys are far shorter; this leaves room
// for a long signed token and refuses a pasted page.
const MaxKeyBytes = 4096

type pastedKey struct {
	mu  sync.Mutex
	key []byte
}

type ctxKeyPasted struct{}

// IsPastedKey reports whether a request's body is a pasted key.
func IsPastedKey(h http.Header) bool { return h.Get(PastedKeyHeader) != "" }

// WithPastedKey puts a request's body on its context.
func WithPastedKey(ctx context.Context, key []byte) context.Context {
	return context.WithValue(ctx, ctxKeyPasted{}, &pastedKey{key: key})
}

// TakePastedKey hands the key over once and leaves nothing behind, so nothing
// further down the request can read it. nil when the request carries none.
// The caller owns the bytes and zeroes them.
func TakePastedKey(ctx context.Context) []byte {
	p, ok := ctx.Value(ctxKeyPasted{}).(*pastedKey)
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := p.key
	p.key = nil
	return key
}

// PutKeyInput is what the managing stack stores.
type PutKeyInput struct {
	Tenant, TenantID string
	Stack            string // the managing stack, for the record
	Principal        string
	Provider         string // the provider, or <provider>:<instance>
	URL              string // for an instance provider: the address the key is for
	Key              []byte
}

// PutKey stores a pasted key: a new connection, or the existing one's key
// replaced. An instance provider's key is bound to the origin of URL, sealed
// with the key, so a later change of address cannot redirect it.
func (s *Service) PutKey(ctx context.Context, in PutKeyInput) (Row, *Error) {
	if !validConnectionName(in.Provider) {
		return Row{}, fail(CodeInvalidArg, "`provider` %q is not a provider name", in.Provider)
	}
	family, instance := splitName(in.Provider)
	_, p, err := s.decls.Provider(ctx, in.Tenant, family)
	if errors.Is(err, ErrNotDeclared) {
		return Row{}, fail(CodeNotDeclared, "no active stack of this tenant declares provider %q (PROVIDERS/%s.yaml)", family, family)
	}
	if err != nil {
		return Row{}, fail(CodeStore, "%v", err)
	}
	if p.Kind != KindAPIKey {
		return Row{}, fail(CodeUnsupported, "provider %q is kind %s: its connections are made by signing in", family, p.Kind)
	}
	if p.Instances != (instance != "") {
		return Row{}, fail(CodeInvalidArg, "provider %q takes %s", family, map[bool]string{true: "<provider>:<instance>", false: "no instance"}[p.Instances])
	}
	origin := ""
	if p.Instances {
		o, oerr := keyOrigin(in.URL)
		if oerr != nil {
			return Row{}, fail(CodeDestination, "%v", oerr)
		}
		origin = o.String()
	}
	if err := checkKey(in.Key); err != nil {
		return Row{}, fail(CodeInvalidArg, "%v", err)
	}
	row, err := s.store.Put(ctx, NewRow{
		TenantID: in.TenantID, PrincipalID: in.Principal, Provider: in.Provider, Kind: CredentialAPIKey,
		Account: origin, CreatedBy: in.Stack,
	}, Material{Access: string(in.Key), Origin: origin})
	if err != nil {
		return Row{}, fail(CodeStore, "%v", err)
	}
	s.event(ctx, "connection.connected", map[string]any{
		"principal": row.PrincipalID, "provider": row.Provider, "account": row.Account, "generation": row.Generation, "stack": in.Stack,
	})
	return row, nil
}

// checkKey accepts what can be sent as one header value. Nothing it says
// quotes the key: this message reaches the envelope.
func checkKey(key []byte) error {
	if len(key) == 0 {
		return errors.New("the request carried no key")
	}
	if len(key) > MaxKeyBytes {
		return errors.New("that is longer than a key can be")
	}
	for _, b := range key {
		if b <= 0x20 || b >= 0x7f {
			return errors.New("a key is one word of plain characters; check that nothing else was pasted with it")
		}
	}
	return nil
}

// ZeroKey clears a key the caller took.
func ZeroKey(key []byte) { secrets.Zero(key) }
