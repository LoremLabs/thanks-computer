package outlet

import (
	"strings"
	"testing"
)

func known(name string) bool { return name == "postgres" }

func TestParseDecl(t *testing.T) {
	d, err := ParseDecl([]byte("driver: postgres\nsecret: CRM_DSN\naccess: write\nmax_rows: 500\ntimeout: 5000\n"), known)
	if err != nil {
		t.Fatalf("valid decl: %v", err)
	}
	if d.Driver != "postgres" || d.Secret != "CRM_DSN" || !d.Writable() || d.MaxRows != 500 || d.Timeout != 5000 {
		t.Fatalf("decoded wrong: %+v", d)
	}

	d, err = ParseDecl([]byte("driver: postgres\nsecret: CRM_DSN\n"), known)
	if err != nil || d.Access != AccessRead || d.Writable() {
		t.Fatalf("default access must be read: %+v %v", d, err)
	}

	bad := map[string]string{
		"unknown key":     "driver: postgres\nsecret: X\nqueries: {}\n",
		"missing driver":  "secret: X\n",
		"unknown driver":  "driver: mysql\nsecret: X\n",
		"missing secret":  "driver: postgres\n",
		"bad secret name": "driver: postgres\nsecret: crm-dsn\n",
		"bad access":      "driver: postgres\nsecret: X\naccess: rw\n",
		"negative rows":   "driver: postgres\nsecret: X\nmax_rows: -1\n",
		"negative time":   "driver: postgres\nsecret: X\ntimeout: -5\n",
	}
	for name, body := range bad {
		if _, err := ParseDecl([]byte(body), known); err == nil {
			t.Errorf("%s: expected an error", name)
		} else if !strings.HasPrefix(err.Error(), "outlet declaration:") {
			t.Errorf("%s: error should be prefixed: %v", name, err)
		}
	}
	// nil knownDriver skips the driver check (runtime path).
	if _, err := ParseDecl([]byte("driver: mysql\nsecret: X\n"), nil); err != nil {
		t.Fatalf("nil knownDriver must not check drivers: %v", err)
	}
}

func TestPaths(t *testing.T) {
	cases := map[string]string{
		"OUTLETS/crm.yaml":      "crm",
		"OUTLETS/crm_ro-2.yaml": "crm_ro-2",
		"OUTLETS/Crm.yaml":      "",
		"OUTLETS/a/b.yaml":      "",
		"OUTLETS/crm.json":      "",
		"OUTLETS/.yaml":         "",
		"outlets/crm.yaml":      "",
		"FILES/crm.yaml":        "",
	}
	for p, want := range cases {
		if got := Name(p); got != want {
			t.Errorf("Name(%q) = %q, want %q", p, got, want)
		}
	}
	if DeclPath("crm") != "OUTLETS/crm.yaml" || !IsOutletPath("OUTLETS/x") || IsOutletPath("OUTLETSX/y") {
		t.Fatal("path helpers")
	}
}
