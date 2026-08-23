package blob

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
)

// kvIndex is the v1 Index over the tenant KV store (boltdb locally, redis on
// the fleet — shared wherever the KV is). Rows are small JSON values in two
// reserved namespaces; names become keys by substituting ':' for '/' (the KV
// forbids '/' in a key segment; ':' is outside the name charset, so the
// substitution is reversible). Listing reads the whole namespace and sorts
// in memory — the same class of cost as kv.ListKeysPage.
type kvIndex struct {
	kv *kvstore.KV
}

// NewKVIndex returns the KV-backed Index.
func NewKVIndex(k *kvstore.KV) Index { return &kvIndex{kv: k} }

const (
	nameKeyPrefix = "n:"
	shaKeyPrefix  = "s:"
)

func nameKey(name string) string { return nameKeyPrefix + strings.ReplaceAll(name, "/", ":") }
func keyName(key string) string {
	return strings.ReplaceAll(strings.TrimPrefix(key, nameKeyPrefix), ":", "/")
}
func shaKey(sha string) string { return shaKeyPrefix + sha }

func (x *kvIndex) GetName(ctx context.Context, tenant, name string) (NameRow, bool, error) {
	raw, found, err := x.kv.Get(ctx, tenant, IndexNamespace, nameKey(name))
	if err != nil || !found {
		return NameRow{}, false, err
	}
	var row NameRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return NameRow{}, false, fmt.Errorf("blob index: decode %q: %w", name, err)
	}
	row.Name = name
	return row, true, nil
}

func (x *kvIndex) PutName(ctx context.Context, tenant string, row NameRow) error {
	if err := ValidName(row.Name); err != nil {
		return err
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return x.kv.Set(ctx, tenant, IndexNamespace, nameKey(row.Name), raw, 0)
}

func (x *kvIndex) DeleteName(ctx context.Context, tenant, name string) error {
	return x.kv.Delete(ctx, tenant, IndexNamespace, nameKey(name))
}

func (x *kvIndex) ListNames(ctx context.Context, tenant string, opts ListOpts) (ListPage, error) {
	pairs, err := x.kv.ListPairs(ctx, tenant, IndexNamespace)
	if err != nil {
		return ListPage{}, err
	}
	rows := make([]NameRow, 0, len(pairs))
	for _, p := range pairs {
		if !strings.HasPrefix(p.Key, nameKeyPrefix) {
			continue
		}
		name := keyName(p.Key)
		if !strings.HasPrefix(name, opts.Prefix) {
			continue
		}
		var row NameRow
		if json.Unmarshal(p.Value, &row) != nil {
			continue // a corrupt row is invisible, like an unreadable KV value
		}
		row.Name = name
		rows = append(rows, row)
	}
	// Sort by the RAW name (not the substituted key — ':' and '/' order
	// differently against digits), so the cursor is a stable resume point.
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	i := sort.Search(len(rows), func(j int) bool { return rows[j].Name > opts.After })
	rows = rows[i:]
	page := ListPage{Names: rows}
	if opts.Limit > 0 && len(rows) > opts.Limit {
		page.Names = rows[:opts.Limit]
		page.Next = rows[opts.Limit-1].Name
	}
	if page.Names == nil {
		page.Names = []NameRow{}
	}
	return page, nil
}

func (x *kvIndex) GetSha(ctx context.Context, tenant, sha string) (ShaRow, bool, error) {
	raw, found, err := x.kv.Get(ctx, tenant, ShaNamespace, shaKey(sha))
	if err != nil || !found {
		return ShaRow{}, false, err
	}
	var row ShaRow
	if err := json.Unmarshal(raw, &row); err != nil {
		return ShaRow{}, false, fmt.Errorf("blob index: decode sha %s: %w", sha, err)
	}
	row.SHA256 = sha
	return row, true, nil
}

func (x *kvIndex) PutShaIfAbsent(ctx context.Context, tenant string, row ShaRow) (bool, error) {
	if !ValidSha256(row.SHA256) {
		return false, fmt.Errorf("blob index: malformed sha256 %q", row.SHA256)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return false, err
	}
	// CAS-if-absent: a concurrent first put of the same bytes keeps the
	// earlier FirstSeen instead of last-writer-wins on an ownership record.
	swapped, _, err := x.kv.CAS(ctx, tenant, ShaNamespace, shaKey(row.SHA256), true, nil, raw, 0)
	return swapped, err
}
