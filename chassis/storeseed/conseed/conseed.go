// Package conseed is the CONTACTS/ store-seed Materializer: it reconciles
// CONTACTS/<username>/<addressbook>.jsonl packs (chassis/storeseed) into the
// contacts store (chassis/contacts) the `contacts` personality serves.
//
// A pack OWNS one address book of one account (managed scope). It is NDJSON:
//
//	{"addressbook":{"display_name":"…","description":"…","sort_order":0,"policy":{…}}}
//	{"name":"bob.vcf","uid":"…","card":{"fn":"Bob Example","emails":[{"value":"bob@example.com"}]}}
//	{"name":"ann.vcf","vcard":"BEGIN:VCARD\r\nVERSION:3.0\r\n…"}
//
// The optional `addressbook` line (at most one, anywhere) sets the book's
// display fields; every other line is one object — `card{}` (the chassis
// renders vCard 3.0; `uid` defaults to <name>.<local>@<domain>) or `vcard`
// text kept as written. Reconcile ensures the book, puts every object
// (unchanged content is a no-op), and deletes every live object in the book
// whose UID the pack no longer lists.
//
// The ACCOUNT must already exist — only an op mints a password — and belong
// to the tenant; otherwise the pack is an error (logged, activation
// unaffected, retried next apply). An address book this materializer
// CREATES gets a policy that denies client `put`/`delete` unless the header
// says otherwise: a client edit in a pack-owned book would be undone by the
// next apply. Keep runtime-written books (OnePony's `senders`) out of packs.
package conseed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// header is the pack's `addressbook` line.
type header struct {
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	SortOrder   int             `json:"sort_order"`
	Policy      json.RawMessage `json:"policy"`
}

// item is one object line.
type item struct {
	Name  string          `json:"name"`
	UID   string          `json:"uid"`
	Card  json.RawMessage `json:"card"`
	VCard string          `json:"vcard"`
}

type line struct {
	Addressbook json.RawMessage `json:"addressbook"`
	item
}

// Materializer reconciles CONTACTS/ packs into a *contacts.Store.
type Materializer struct {
	store  *chcon.Store
	shared bool
	now    func() time.Time
}

// New builds the contacts Materializer. shared declares whether the store
// backend is fleet-shared (postgres) — reconciled once on the origin — or
// per-node (sqlite). The wiring layer (server.go) knows the backend.
func New(store *chcon.Store, shared bool) *Materializer {
	return &Materializer{store: store, shared: shared, now: func() time.Time { return time.Now().UTC() }}
}

func (m *Materializer) Kind() string { return storeseed.KindContact }
func (m *Materializer) Shared() bool { return m.shared }

// Reconcile makes each pack's address book match the pack. Errors are
// aggregated per pack so one bad pack does not skip the rest.
func (m *Materializer) Reconcile(ctx context.Context, scope storeseed.Scope, packs []storeseed.RawPack) error {
	if m.store == nil {
		return errors.New("contacts store not configured")
	}
	var errs []error
	for _, p := range packs {
		if p.Path == "" {
			continue // EmptyTree marker: single-file kinds keep "pack removed = stop managing"
		}
		if err := m.reconcileOne(ctx, scope, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Path, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Materializer) reconcileOne(ctx context.Context, scope storeseed.Scope, p storeseed.RawPack) error {
	username, bookName, ok := storeseed.ContactsPackName(p.Name)
	if !ok || !chcon.ValidAddressbookName(bookName) {
		return fmt.Errorf("pack name %q is not <username>/<addressbook>", p.Name)
	}
	username = chcon.NormalizeUsername(username)
	acct, found, err := m.store.GetAccount(ctx, username)
	if err != nil {
		return fmt.Errorf("account: %w", err)
	}
	if !found {
		return fmt.Errorf("no contacts account %q — provision it with txco://contacts/account first (a pack cannot mint a password)", username)
	}
	if acct.Tenant != scope.Tenant {
		return fmt.Errorf("account %q belongs to another tenant", username)
	}
	hdr, items, err := parsePack(p)
	if err != nil {
		return err
	}
	ab := chcon.Addressbook{Tenant: scope.Tenant, Username: username, Name: bookName}
	if hdr != nil {
		ab.DisplayName, ab.Description, ab.SortOrder = hdr.DisplayName, hdr.Description, hdr.SortOrder
		if len(hdr.Policy) > 0 {
			if err := chcon.ValidatePolicy(hdr.Policy); err != nil {
				return fmt.Errorf("addressbook.policy: %w", err)
			}
			ab.Policy = hdr.Policy
		}
	}
	if _, exists, gerr := m.store.GetAddressbook(ctx, scope.Tenant, username, bookName); gerr != nil {
		return fmt.Errorf("addressbook: %w", gerr)
	} else if !exists && len(ab.Policy) == 0 {
		// A pack-owned address book is not a client's to edit.
		ab.Policy = json.RawMessage(`{"put":"deny","delete":"deny"}`)
	}
	ensured, _, err := m.store.EnsureAddressbook(ctx, ab)
	if err != nil {
		return fmt.Errorf("ensure addressbook: %w", err)
	}
	now := m.now()
	keep := map[string]struct{}{}
	var errs []error
	for i, it := range items {
		var res chcon.PutResult
		var perr error
		if it.VCard != "" {
			res, perr = m.store.PutVCard(ctx, ensured.ID, it.Name, []byte(it.VCard))
		} else {
			c, cerr := chcon.CardFromJSON(it.Card)
			if cerr != nil {
				errs = append(errs, fmt.Errorf("line %d: %w", i+1, cerr))
				continue
			}
			if c.UID == "" {
				c.UID = it.UID
			}
			res, perr = m.store.PutCard(ctx, ensured.ID, username, it.Name, c, now)
		}
		if perr != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", i+1, perr))
			continue
		}
		keep[res.UID] = struct{}{}
	}
	// Delete-missing: the pack is the desired state of this address book.
	live, err := m.store.ListObjects(ctx, ensured.ID, chcon.ListOpts{})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list objects: %w", err))...)
	}
	for _, o := range live {
		if _, ok := keep[o.UID]; ok {
			continue
		}
		if _, _, derr := m.store.DeleteObject(ctx, ensured.ID, o.Name); derr != nil {
			errs = append(errs, fmt.Errorf("delete stale %q: %w", o.Name, derr))
		}
	}
	return errors.Join(errs...)
}

// parsePack decodes the pack: at most one `addressbook` header, then object
// lines each carrying `card` or `vcard`. Malformed lines fail the pack.
func parsePack(p storeseed.RawPack) (*header, []item, error) {
	var hdr *header
	var items []item
	for i, raw := range p.Lines() {
		var ln line
		if err := json.Unmarshal(raw, &ln); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if len(ln.Addressbook) > 0 && string(ln.Addressbook) != "null" {
			if hdr != nil {
				return nil, nil, fmt.Errorf("line %d: a second `addressbook` header", i+1)
			}
			var h header
			if err := json.Unmarshal(ln.Addressbook, &h); err != nil {
				return nil, nil, fmt.Errorf("line %d: addressbook: %w", i+1, err)
			}
			hdr = &h
			continue
		}
		hasCard := len(ln.Card) > 0 && string(ln.Card) != "null"
		hasVCard := strings.TrimSpace(ln.VCard) != ""
		if hasCard == hasVCard {
			return nil, nil, fmt.Errorf("line %d: give `card` or `vcard`, not both or neither", i+1)
		}
		if hasCard && !json.Valid(ln.Card) {
			return nil, nil, fmt.Errorf("line %d: card is not valid JSON", i+1)
		}
		if hasCard && ln.UID == "" && ln.Name == "" {
			return nil, nil, fmt.Errorf("line %d: a card needs a `name` or a `uid`", i+1)
		}
		items = append(items, ln.item)
	}
	return hdr, items, nil
}
