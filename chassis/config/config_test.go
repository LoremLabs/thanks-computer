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
