package allowance

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/kv"
)

// Chassis-reserved KV namespaces (kv.IsReservedNamespace), so the author-
// facing txco://kv/* ops refuse them: definitions keyed by name, counters
// keyed by "<name>@<window id>".
const (
	NamespaceDefs = kv.ReservedNamespacePrefix + ".allowance"
	NamespaceUsed = kv.ReservedNamespacePrefix + ".allowance.used"
)

// counterGrace keeps a window's counter readable a while past the window's
// end, for a late charge or a status read across the boundary.
const counterGrace = time.Hour

// defTTL bounds how long Charge trusts a cached definition's period. Status
// always reads the definition fresh (it is the check), so this only decides
// which window a charge lands in for a few seconds after a period change.
const defTTL = 10 * time.Second

// Store reads and writes allowances in a tenant's KV. Safe for concurrent use.
type Store struct {
	kv  *kv.KV
	now func() time.Time

	mu   sync.Mutex
	defs map[string]cachedDef // tenant + "/" + name
}

type cachedDef struct {
	def   Def
	found bool
	at    time.Time
}

// NewStore returns a Store over k. Pass a kv.KV with no TTL ceiling: a month
// window's counter must outlive any operator cap on author TTLs.
func NewStore(k *kv.KV) *Store {
	return &Store{kv: k, now: time.Now, defs: map[string]cachedDef{}}
}

func counterKey(name, windowID string) string { return name + "@" + windowID }

// Set creates or replaces a definition. Its counters are kept: raising the
// limit mid-window frees what is left of the window at once.
func (s *Store) Set(ctx context.Context, tenant string, d Def) error {
	if err := d.Validate(); err != nil {
		return err
	}
	b, _ := json.Marshal(d)
	if err := s.kv.Set(ctx, tenant, NamespaceDefs, d.Name, b, 0); err != nil {
		return err
	}
	s.remember(tenant, d.Name, d, true)
	return nil
}

// Def reads one definition (fresh, not cached).
func (s *Store) Def(ctx context.Context, tenant, name string) (Def, bool, error) {
	if !ValidName(name) {
		return Def{}, false, fmt.Errorf("allowance: invalid name %q", name)
	}
	raw, found, err := s.kv.Get(ctx, tenant, NamespaceDefs, name)
	if err != nil || !found {
		return Def{}, false, err
	}
	var d Def
	if err := json.Unmarshal(raw, &d); err != nil {
		return Def{}, false, fmt.Errorf("allowance: corrupt definition %q: %w", name, err)
	}
	s.remember(tenant, name, d, true)
	return d, true, nil
}

// Delete removes a definition. Its counters expire on their own.
func (s *Store) Delete(ctx context.Context, tenant, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("allowance: invalid name %q", name)
	}
	if err := s.kv.Delete(ctx, tenant, NamespaceDefs, name); err != nil {
		return err
	}
	s.remember(tenant, name, Def{}, false)
	return nil
}

// List returns one page of definitions in name order after the cursor;
// next is "" when there are no more.
func (s *Store) List(ctx context.Context, tenant, after string, limit int) (defs []Def, next string, err error) {
	pairs, next, err := s.kv.ListPairsPage(ctx, tenant, NamespaceDefs, after, limit)
	if err != nil {
		return nil, "", err
	}
	defs = make([]Def, 0, len(pairs))
	for _, p := range pairs {
		var d Def
		if json.Unmarshal(p.Value, &d) != nil {
			continue // a corrupt row is skipped, never fatal to the listing
		}
		defs = append(defs, d)
	}
	return defs, next, nil
}

// Status reads a definition and its current window's counter in one round
// trip. The counter key depends on the period, which depends on the
// definition, so all three windows' counters are read alongside it.
func (s *Store) Status(ctx context.Context, tenant, name string) (Status, error) {
	if !ValidName(name) {
		return Status{}, fmt.Errorf("allowance: invalid name %q", name)
	}
	now := s.now()
	periods := []Period{Hour, Day, Month}
	refs := []kv.Ref{{Namespace: NamespaceDefs, Key: name}}
	for _, p := range periods {
		id, _, _ := p.Window(now)
		refs = append(refs, kv.Ref{Namespace: NamespaceUsed, Key: counterKey(name, id)})
	}
	hits, err := s.kv.GetMany(ctx, tenant, refs)
	if err != nil {
		return Status{}, err
	}
	st := Status{Name: name, Per: DefaultPeriod}
	if hits[0].Found {
		var d Def
		if err := json.Unmarshal(hits[0].Value, &d); err != nil {
			return Status{}, fmt.Errorf("allowance: corrupt definition %q: %w", name, err)
		}
		st.Defined, st.Fuel, st.Per = true, d.Fuel, d.Per
		s.remember(tenant, name, d, true)
	} else {
		s.remember(tenant, name, Def{}, false)
	}
	for i, p := range periods {
		if p != st.Per {
			continue
		}
		if h := hits[i+1]; h.Found {
			_ = json.Unmarshal(h.Value, &st.Used)
		}
		_, _, st.ResetsAt = p.Window(now)
	}
	return st, nil
}

// Charge adds fuel to the allowance's current window. The window follows the
// definition's period (cached briefly); an undefined allowance is counted by
// DefaultPeriod.
func (s *Store) Charge(ctx context.Context, tenant, name string, fuel int64) error {
	if fuel <= 0 {
		return nil
	}
	per, err := s.period(ctx, tenant, name)
	if err != nil {
		return err
	}
	now := s.now()
	id, _, end := per.Window(now)
	_, err = s.kv.Incr(ctx, tenant, NamespaceUsed, counterKey(name, id), fuel, end.Sub(now)+counterGrace)
	return err
}

func (s *Store) period(ctx context.Context, tenant, name string) (Period, error) {
	s.mu.Lock()
	c, ok := s.defs[tenant+"/"+name]
	s.mu.Unlock()
	if ok && s.now().Sub(c.at) < defTTL {
		if c.found {
			return c.def.Per, nil
		}
		return DefaultPeriod, nil
	}
	d, found, err := s.Def(ctx, tenant, name)
	if err != nil {
		return "", err
	}
	if !found {
		s.remember(tenant, name, Def{}, false)
		return DefaultPeriod, nil
	}
	return d.Per, nil
}

// maxCachedDefs bounds the period cache; past it the cache is dropped and
// refilled, which costs a definition read per allowance on the next flush.
const maxCachedDefs = 10000

func (s *Store) remember(tenant, name string, d Def, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.defs) >= maxCachedDefs {
		s.defs = map[string]cachedDef{}
	}
	s.defs[tenant+"/"+name] = cachedDef{def: d, found: found, at: s.now()}
}
