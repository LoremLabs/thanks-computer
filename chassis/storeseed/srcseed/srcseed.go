// Package srcseed is the SOURCES/ store-seed Materializer: it reconciles
// SOURCES/<pack>.jsonl packs (chassis/storeseed) into the tenant_sources
// runtime table (chassis/source). A pack line DECLARES a remote source the
// tenant wants watched:
//
//	{"id":"support","kind":"imap","host":"imap.fastmail.com","port":993,
//	 "tls":"implicit","user":"desk@acme.com","secret":"ACME_DESK_MAILBOX",
//	 "mailbox":"INBOX","every":"5m","on_processed":"move:Processed"}
//
// `secret` is a NAME (resolved from the encrypted secret store at poll time);
// a value never appears in a pack, a row, or a log. Each pack OWNS its rows:
// reconcile UPSERTs the declared columns of every line and SOFT-RETIRES any
// managed source of that pack whose id is no longer present, so a redeploy
// that drops a line stops the watcher without discarding its cursor (re-adding
// it resumes rather than re-reading the whole mailbox).
//
// The materializer writes ONLY the declared columns; the runtime columns
// (cursor, claim, next_poll_at) belong to the poller. That split is what makes
// a redeploy safe to run against a live source.
package srcseed

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/source"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// Materializer reconciles SOURCES packs into a *source.Store.
type Materializer struct {
	store *source.Store
}

// New builds the SOURCES Materializer over the runtime-DB source store.
func New(store *source.Store) *Materializer { return &Materializer{store: store} }

func (m *Materializer) Kind() string { return storeseed.KindSource }

// Shared is always true: tenant_sources lives in the shared runtime DB (one
// Postgres on a fleet, one SQLite file on a single node), so it is reconciled
// exactly once — on the activation origin — and never raced by data-plane
// appliers.
func (m *Materializer) Shared() bool { return true }

func (m *Materializer) Reconcile(ctx context.Context, scope storeseed.Scope, packs []storeseed.RawPack) error {
	for _, p := range packs {
		if err := m.reconcileOne(ctx, scope, p); err != nil {
			return fmt.Errorf("pack %q: %w", p.Name, err)
		}
	}
	return nil
}

// packLine is the subset of a source declaration the materializer promotes to
// filterable columns; the whole line is also stored verbatim as `config` for
// the poller and kind to read (they ignore the fields below).
type packLine struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Every   json.RawMessage `json:"every"`   // "5m" | "300s" | 300 (seconds)
	Enabled *bool           `json:"enabled"` // default true
}

func (m *Materializer) reconcileOne(ctx context.Context, scope storeseed.Scope, p storeseed.RawPack) error {
	keep := make([]string, 0)
	seen := map[string]struct{}{}
	for i, line := range p.Lines() {
		var pl packLine
		if err := json.Unmarshal(line, &pl); err != nil {
			return fmt.Errorf("line %d: %w", i+1, err)
		}
		if pl.ID == "" {
			return fmt.Errorf("line %d: missing id", i+1)
		}
		if _, dup := seen[pl.ID]; dup {
			return fmt.Errorf("line %d: duplicate id %q", i+1, pl.ID)
		}
		seen[pl.ID] = struct{}{}

		kind := pl.Kind
		if kind == "" {
			kind = "imap"
		}
		enabled := pl.Enabled == nil || *pl.Enabled
		every := parseEvery(pl.Every)

		if err := m.store.Upsert(ctx, source.Declared{
			Tenant:       scope.Tenant,
			Stack:        scope.Stack,
			Pack:         p.Name,
			DeclaredID:   pl.ID,
			Kind:         kind,
			Config:       json.RawMessage(line), // the pack line, verbatim (secret is a name)
			Enabled:      enabled,
			EverySeconds: every,
			Version:      scope.Version,
		}); err != nil {
			return fmt.Errorf("line %d (id %q): %w", i+1, pl.ID, err)
		}
		keep = append(keep, pl.ID)
	}

	// Desired-state: retire any managed source of this pack whose id the new
	// version no longer declares (keep empty ⇒ the pack emptied ⇒ retire all).
	if _, err := m.store.RetireMissing(ctx, scope.Tenant, scope.Stack, p.Name, keep); err != nil {
		return err
	}
	return nil
}

// parseEvery reads the poll interval: a duration string ("5m", "90s", "1h") or
// a bare number of seconds. Anything unparseable falls back to 300s (5m), the
// documented default — a malformed interval must not disable a source.
func parseEvery(raw json.RawMessage) int {
	const def = 300
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return def
	}
	// Quoted string form: "5m" / "300s".
	if strings.HasPrefix(s, `"`) {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			if d, err := time.ParseDuration(strings.TrimSpace(str)); err == nil && d >= time.Second {
				return int(d.Seconds())
			}
			// bare number inside quotes?
			if n, err := strconv.Atoi(strings.TrimSpace(str)); err == nil && n > 0 {
				return n
			}
		}
		return def
	}
	// Numeric seconds.
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}
