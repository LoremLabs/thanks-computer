package config

import (
	"os"
	"testing"
)

// TestLoadFromFlagsAndEnv covers the viper+pflag loader (replaced gonfig).
// Verifies precedence (flag > env > default) for representative types:
// string, int, bool, []string. A regression here breaks every operator's
// flag/env-var workflow, so it's worth a smoke test.
func TestLoadFromFlagsAndEnv(t *testing.T) {
	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })

	// Flag override wins for these keys.
	os.Args = []string{
		"chassis",
		"--web-addr=:9999",
		"--cron-period=30",
		"--repl",
		"--tcp-listen-addrs=:7000,:7001",
	}
	// Env override wins for keys not on the command line.
	t.Setenv("TXCO_ENV", "smoke")
	t.Setenv("TXCO_DB_ROOT_DIR", "/tmp/test-db-root")

	var c Config
	if err := loadFromFlagsAndEnv(&c); err != nil {
		t.Fatalf("loadFromFlagsAndEnv: %v", err)
	}

	// Flag-overridden values
	if c.WebAddr != ":9999" {
		t.Errorf("WebAddr = %q, want :9999", c.WebAddr)
	}
	if c.CronPeriod != 30 {
		t.Errorf("CronPeriod = %d, want 30", c.CronPeriod)
	}
	if !c.Repl {
		t.Errorf("Repl = false, want true")
	}
	if got := c.TCPListenAddrs; len(got) != 2 || got[0] != ":7000" || got[1] != ":7001" {
		t.Errorf("TCPListenAddrs = %v, want [:7000 :7001]", got)
	}

	// Env-overridden values
	if c.Environment != "smoke" {
		t.Errorf("Environment = %q, want smoke (from TXCO_ENV)", c.Environment)
	}
	if c.DbRoot != "/tmp/test-db-root" {
		t.Errorf("DbRoot = %q, want /tmp/test-db-root (from TXCO_DB_ROOT_DIR)", c.DbRoot)
	}

	// Default values (untouched by flag or env)
	if c.AdminAddr != ":8081" {
		t.Errorf("AdminAddr = %q, want default :8081", c.AdminAddr)
	}
}

// TestLoadFromFlagsAndEnv_ListEnvSeparators pins the separator contract for
// list-valued settings coming from the ENVIRONMENT. A CLI flag splits on
// commas (pflag StringSlice CSV); an env value reaches viper as one string
// and used to be split on whitespace only, so TXCO_IMAP_PROXY_PROTOCOL=
// "172.16.0.0/12,fdaa::/8" arrived as ONE unparseable entry (first IMAP 0c
// deploy, 2026-09-03). splitListEntries makes the documented comma form
// work from env too, keeps whitespace working, and drops blanks.
func TestLoadFromFlagsAndEnv_ListEnvSeparators(t *testing.T) {
	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })
	os.Args = []string{"chassis"}

	t.Setenv("TXCO_IMAP_PROXY_PROTOCOL", "172.16.0.0/12,fdaa::/8") // comma
	t.Setenv("TXCO_LMTP_LISTEN_ADDRS", ":2424 :2425")              // whitespace
	t.Setenv("TXCO_DNS_NAMESERVERS", "ns1.example, ns2.example ,") // mixed + blanks
	t.Setenv("TXCO_IMAP_LISTEN_ADDRS", ":1143")                    // single

	var c Config
	if err := loadFromFlagsAndEnv(&c); err != nil {
		t.Fatalf("loadFromFlagsAndEnv: %v", err)
	}
	want := map[string][]string{
		"IMAPProxyProtocol": {"172.16.0.0/12", "fdaa::/8"},
		"LMTPListenAddrs":   {":2424", ":2425"},
		"DNSNameservers":    {"ns1.example", "ns2.example"},
		"IMAPListenAddrs":   {":1143"},
	}
	got := map[string][]string{
		"IMAPProxyProtocol": c.IMAPProxyProtocol,
		"LMTPListenAddrs":   c.LMTPListenAddrs,
		"DNSNameservers":    c.DNSNameservers,
		"IMAPListenAddrs":   c.IMAPListenAddrs,
	}
	for k, w := range want {
		g := got[k]
		if len(g) != len(w) {
			t.Errorf("%s = %q, want %q", k, g, w)
			continue
		}
		for i := range w {
			if g[i] != w[i] {
				t.Errorf("%s[%d] = %q, want %q", k, i, g[i], w[i])
			}
		}
	}
	// A list flag left unset stays empty (no [""] from splitting "").
	if len(c.IMAPTLSAddrs) != 0 {
		t.Errorf("IMAPTLSAddrs = %q, want empty", c.IMAPTLSAddrs)
	}
}
