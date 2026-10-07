package server

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/allowance"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

type allowanceHandler func(context.Context, *allowance.Store, []byte) (event.Payload, error)

func callAllowance(t *testing.T, fn allowanceHandler, s *allowance.Store, tenant, meta string) (event.Payload, error) {
	t.Helper()
	ctx := operation.WithMeta(context.Background(), meta)
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	return fn(ctx, s, []byte(`{}`))
}

func TestAllowanceOpsRoundTrip(t *testing.T) {
	s := allowance.NewStore(newKVHandle(t))

	p, err := callAllowance(t, allowanceSet, s, "acme", `{"name":"scout","fuel":500,"per":"hour"}`)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := gjson.Get(p.Raw, "_allowance.per").String(); got != "hour" {
		t.Errorf("set result per = %q: %s", got, p.Raw)
	}
	if _, err := callAllowance(t, allowanceSet, s, "acme", `{"name":"bard","fuel":10}`); err != nil {
		t.Fatalf("set with default per: %v", err)
	}

	p, err = callAllowance(t, allowanceGet, s, "acme", `{"name":"scout","into":"st"}`)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !gjson.Get(p.Raw, "st.defined").Bool() || gjson.Get(p.Raw, "st.fuel").Int() != 500 ||
		gjson.Get(p.Raw, "st.remaining").Int() != 500 || !gjson.Get(p.Raw, "st.resets_at").Exists() {
		t.Errorf("get (into st) = %s", p.Raw)
	}

	p, err = callAllowance(t, allowanceList, s, "acme", `{}`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if n := gjson.Get(p.Raw, "_allowance.count").Int(); n != 2 ||
		gjson.Get(p.Raw, "_allowance.allowances.0.name").String() != "bard" ||
		gjson.Get(p.Raw, "_allowance.allowances.0.per").String() != "day" {
		t.Errorf("list = %s", p.Raw)
	}

	// Tenant-scoped by the pin: another tenant sees nothing.
	p, _ = callAllowance(t, allowanceList, s, "other", `{}`)
	if n := gjson.Get(p.Raw, "_allowance.count").Int(); n != 0 {
		t.Errorf("another tenant lists %d allowances", n)
	}

	if _, err := callAllowance(t, allowanceDelete, s, "acme", `{"name":"scout"}`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	p, _ = callAllowance(t, allowanceGet, s, "acme", `{"name":"scout"}`)
	if gjson.Get(p.Raw, "_allowance.defined").Bool() || gjson.Get(p.Raw, "_allowance.remaining").Exists() {
		t.Errorf("deleted allowance still limited: %s", p.Raw)
	}
}

func TestAllowanceOpsRefuse(t *testing.T) {
	s := allowance.NewStore(newKVHandle(t))
	for _, c := range []struct {
		name   string
		fn     allowanceHandler
		store  *allowance.Store
		tenant string
		meta   string
	}{
		{"set without a tenant", allowanceSet, s, "", `{"name":"scout","fuel":5}`},
		{"set with no fuel", allowanceSet, s, "acme", `{"name":"scout"}`},
		{"set with a bad period", allowanceSet, s, "acme", `{"name":"scout","fuel":5,"per":"week"}`},
		{"set with a bad name", allowanceSet, s, "acme", `{"name":"Scout/1","fuel":5}`},
		{"get with no name outside an allowance", allowanceGet, s, "acme", `{}`},
		{"enter with no name", allowanceEnter, s, "acme", `{}`},
		{"enter with no request budget", allowanceEnter, s, "acme", `{"name":"scout"}`},
		{"any op without a KV store", allowanceList, nil, "acme", `{}`},
	} {
		p, err := callAllowance(t, c.fn, c.store, c.tenant, c.meta)
		if err == nil {
			t.Errorf("%s: no error (result %s)", c.name, p.Raw)
			continue
		}
		if got := gjson.Get(p.Meta, "error.0").String(); got != "allowance-err" {
			t.Errorf("%s: error meta = %q, want allowance-err", c.name, p.Meta)
		}
	}
}

func TestAdmissionBilling(t *testing.T) {
	for _, c := range []struct {
		payload          string
		denied, billable bool
	}{
		{`{}`, false, true},
		{`{"_txc":{"admission":{"denied":true,"status":429,"reason":"rate_limited"}}}`, true, false},
		{`{"_txc":{"admission":{"denied":true,"status":402,"reason":"credit_exhausted"}}}`, true, false},
		{`{"_txc":{"admission":{"denied":true,"status":429,"reason":"allowance_exhausted"}}}`, true, true},
	} {
		denied, _, billable := admissionBilling([]byte(c.payload))
		if denied != c.denied || billable != c.billable {
			t.Errorf("admissionBilling(%s) = denied %v billable %v, want %v %v", c.payload, denied, billable, c.denied, c.billable)
		}
	}
}

// Allowance definitions and counters sit in chassis-reserved KV namespaces:
// a stack's txco://kv/* ops can neither reset a counter nor forge a limit.
func TestKVOpsRefuseAllowanceNamespaces(t *testing.T) {
	k := newKVHandle(t)
	for _, ns := range []string{allowance.NamespaceDefs, allowance.NamespaceUsed} {
		meta := `{"namespace":"` + ns + `","key":"scout@d20261007","value":0}`
		if _, err := callKV(t, kvSet, k, "acme", "app", meta, ""); err == nil {
			t.Errorf("kv/set into %s succeeded", ns)
		}
	}
}
