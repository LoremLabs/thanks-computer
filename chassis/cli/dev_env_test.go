package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envOf flattens an env slice the way the child process reads it: the LAST
// assignment of a name wins.
func envOf(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

func noParentEnv(string) (string, bool) { return "", false }

func parentEnv(set map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := set[k]; return v, ok }
}

var devAddrs = chassisAddrs{Admin: ":18081", Web: ":18080", TCP: ":5050", DNS: ":5353"}

func TestChassisEnvDefaults(t *testing.T) {
	ws := t.TempDir()
	devDir := filepath.Join(ws, ".txco", "dev")
	env, heads := chassisEnv(chassisOpts{Workspace: ws}, devAddrs, devDir, "/schema", noParentEnv)
	got := envOf(env)

	if h := strings.Join(heads, ","); h != "cron,web,admin,websocket" || got["TXCO_PERSONALITIES"] != h {
		t.Errorf("heads = %q, TXCO_PERSONALITIES = %q", h, got["TXCO_PERSONALITIES"])
	}
	for k, want := range map[string]string{
		"TXCO_ADMIN_ADDR":             ":18081",
		"TXCO_WEB_ADDR":               ":18080",
		"TXCO_SIGNED_URL_BASE":        "http://localhost:18080",
		"TXCO_DB_ROOT_DIR":            filepath.Join(devDir, "db"),
		"TXCO_DB_SCHEMA_DIR":          "/schema",
		"TXCO_SYSTEM_OPSTACKS_DIR":    ws,
		"TXCO_AUTH_MODE":              "basic",
		"TXCO_EGRESS_POLICY":          "open",
		"TXCO_LOG_LEVEL":              "info",
		"TXCO_TRACE_MODE":             "full",
		"TXCO_STRUCTURED_HOST_SUFFIX": ".localhost",
		// Set twice on the way; the dev-scoped path is the one that lands.
		"TXCO_SECRET_MASTER_KEY": filepath.Join(devDir, "secrets", "txco-dev-master.key"),
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// Every path the chassis would otherwise default into ./chassis/data
	// lands under .txco/dev, so the workspace stays clean.
	for _, k := range []string{
		"TXCO_KVSTORE_ADDRS", "TXCO_LOG_OPS_DIR", "TXCO_REPOSTORE_FILE_DIR", "TXCO_CONTINUATION_STORE_FILE_DIR",
		"TXCO_ARTIFACT_STORE_FILE_DIR", "TXCO_FILECAS_STORE_FILE_DIR", "TXCO_VECTOR_DB_PATH",
		"TXCO_NOTEBOOK_DB_PATH", "TXCO_TRACE_DIR",
	} {
		if !strings.HasPrefix(got[k], devDir+string(filepath.Separator)) {
			t.Errorf("%s = %q, want a path under %s", k, got[k], devDir)
		}
	}
	// Nothing an unselected head needs is set, and nothing unsafe is implied.
	for _, k := range []string{
		"TXCO_TCP_LISTEN_ADDRS", "TXCO_DNS_LISTEN_ADDRS", "TXCO_LMTP_LISTEN_ADDRS", "TXCO_IMAP_LISTEN_ADDRS",
		"TXCO_STATE_DB_PATH", "TXCO_WORKSPACE_PROVIDER", "TXCO_WORKSPACE_ALLOW_LOCAL", "TXCO_MAIL_RELAY_ADDR",
		"TXCO_IMAP_INSECURE_AUTH", "TXCO_DRIVE_INSECURE_AUTH",
	} {
		if v, set := got[k]; set {
			t.Errorf("%s = %q with no head selected", k, v)
		}
	}

	// No schema dir found: the variable is left to the chassis's default.
	env, _ = chassisEnv(chassisOpts{Workspace: ws}, devAddrs, devDir, "", noParentEnv)
	if v, set := envOf(env)["TXCO_DB_SCHEMA_DIR"]; set {
		t.Errorf("TXCO_DB_SCHEMA_DIR = %q with no schema dir", v)
	}
}

func TestChassisEnvParentWins(t *testing.T) {
	ws := t.TempDir()
	devDir := filepath.Join(ws, ".txco", "dev")
	parent := parentEnv(map[string]string{
		"TXCO_TRACE_MODE": "off", "TXCO_LOG_LEVEL": "error", "TXCO_AUTH_MODE": "both",
		"TXCO_SECRET_MASTER_KEY": "/elsewhere/key",
	})
	count := func(env []string, name string) (n int) {
		for _, kv := range env {
			if strings.HasPrefix(kv, name+"=") {
				n++
			}
		}
		return n
	}

	// A dev default is set only if the developer did not export it: the
	// child inherits the parent's value, so the default must not be there.
	env, _ := chassisEnv(chassisOpts{Workspace: ws}, devAddrs, devDir, "", parent)
	for _, k := range []string{"TXCO_TRACE_MODE", "TXCO_LOG_LEVEL", "TXCO_AUTH_MODE"} {
		if n := count(env, k); n != 0 {
			t.Errorf("%s is set %d times over the parent's value", k, n)
		}
	}
	if got := envOf(env)["TXCO_EGRESS_POLICY"]; got != "open" {
		t.Errorf("a default the parent did not set: TXCO_EGRESS_POLICY = %q", got)
	}
	if got := envOf(env)["TXCO_SECRET_MASTER_KEY"]; strings.Contains(got, "txco-dev-master.key") {
		t.Errorf("the dev master key path replaced the parent's: %q", got)
	}

	// --verbose beats both.
	env, _ = chassisEnv(chassisOpts{Workspace: ws, Verbose: true}, devAddrs, devDir, "", parent)
	if got := envOf(env)["TXCO_LOG_LEVEL"]; got != "debug" {
		t.Errorf("--verbose: TXCO_LOG_LEVEL = %q", got)
	}
	env, _ = chassisEnv(chassisOpts{Workspace: ws, Verbose: true}, devAddrs, devDir, "", noParentEnv)
	if got := envOf(env)["TXCO_LOG_LEVEL"]; got != "debug" {
		t.Errorf("--verbose over the default: TXCO_LOG_LEVEL = %q", got)
	}
}

func TestChassisEnvHeads(t *testing.T) {
	ws := t.TempDir()
	devDir := filepath.Join(ws, ".txco", "dev")
	for _, tc := range []struct {
		head  string
		heads devHeads
		want  map[string]string
	}{
		{"tcp", devHeads{TCP: true}, map[string]string{"TXCO_TCP_LISTEN_ADDRS": ":5050"}},
		{"dns", devHeads{DNS: true}, map[string]string{"TXCO_DNS_LISTEN_ADDRS": ":5353", "TXCO_DNS_EDGE_IPS": "127.0.0.1"}},
		{"lmtp", devHeads{LMTP: true}, map[string]string{"TXCO_LMTP_LISTEN_ADDRS": devLMTPListenAddr, "TXCO_MAIL_RELAY_ADDR": devMailRelayAddr, "TXCO_MAIL_RELAY_TLS": "none"}},
		{"scheduled", devHeads{Scheduled: true}, map[string]string{"TXCO_SCHEDULED_DB_PATH": filepath.Join(devDir, "scheduled.db")}},
		{"state", devHeads{State: true}, map[string]string{"TXCO_STATE_DB_PATH": filepath.Join(devDir, "state.db")}},
		{"source", devHeads{Source: true}, nil},
		{"imap", devHeads{IMAP: true}, map[string]string{"TXCO_IMAP_LISTEN_ADDRS": devIMAPListenAddr, "TXCO_IMAP_TLS_ADDRS": devIMAPTLSAddr, "TXCO_IMAP_INSECURE_AUTH": "true"}},
		{"calendar", devHeads{Calendar: true}, map[string]string{"TXCO_CALENDAR_DB_PATH": filepath.Join(devDir, "calendar.db"), "TXCO_CALENDAR_INSECURE_AUTH": "true"}},
		{"contacts", devHeads{Contacts: true}, map[string]string{"TXCO_CONTACTS_DB_PATH": filepath.Join(devDir, "contacts.db"), "TXCO_CONTACTS_INSECURE_AUTH": "true"}},
		{"webdav", devHeads{WebDAV: true}, map[string]string{"TXCO_DRIVE_DB_PATH": filepath.Join(devDir, "drive.db"), "TXCO_DRIVE_INSECURE_AUTH": "true"}},
		{"ipp", devHeads{IPP: true}, map[string]string{"TXCO_WEB_TLS_ADDR": devIPPTLSAddr, "TXCO_IPP_DB_PATH": filepath.Join(devDir, "ipp.db"), "TXCO_WEB_TLS_SELF_SIGNED": "true"}},
		{"grant", devHeads{Grant: true}, nil},
	} {
		env, heads := chassisEnv(chassisOpts{Workspace: ws, Heads: tc.heads}, devAddrs, devDir, "", noParentEnv)
		got := envOf(env)
		if want := "cron,web,admin,websocket," + tc.head; strings.Join(heads, ",") != want || got["TXCO_PERSONALITIES"] != want {
			t.Errorf("%s: heads = %v, TXCO_PERSONALITIES = %q", tc.head, heads, got["TXCO_PERSONALITIES"])
		}
		for k, want := range tc.want {
			if got[k] != want {
				t.Errorf("%s: %s = %q, want %q", tc.head, k, got[k], want)
			}
		}
	}

	// Every head at once, in a fixed order.
	all := devHeads{TCP: true, DNS: true, LMTP: true, Scheduled: true, Source: true, IMAP: true,
		Calendar: true, Contacts: true, WebDAV: true, IPP: true, State: true, Grant: true}
	_, heads := chassisEnv(chassisOpts{Workspace: ws, Heads: all}, devAddrs, devDir, "", noParentEnv)
	if got, want := strings.Join(heads, ","), "cron,web,admin,websocket,tcp,dns,lmtp,scheduled,state,source,imap,calendar,contacts,webdav,ipp,grant"; got != want {
		t.Errorf("all heads = %q, want %q", got, want)
	}

	// The lmtp head loads the workspace's ingress.yaml when there is one.
	env, _ := chassisEnv(chassisOpts{Workspace: ws, Heads: devHeads{LMTP: true}}, devAddrs, devDir, "", noParentEnv)
	if v, set := envOf(env)["TXCO_INGRESS_CONFIG"]; set {
		t.Errorf("TXCO_INGRESS_CONFIG = %q with no ingress.yaml", v)
	}
	ing := filepath.Join(ws, "ingress.yaml")
	if err := os.WriteFile(ing, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _ = chassisEnv(chassisOpts{Workspace: ws, Heads: devHeads{LMTP: true}}, devAddrs, devDir, "", noParentEnv)
	if got := envOf(env)["TXCO_INGRESS_CONFIG"]; got != ing {
		t.Errorf("TXCO_INGRESS_CONFIG = %q, want %q", got, ing)
	}
}

// The grant socket's path is the chassis's to choose: one under .txco/dev
// could be too long for a socket. And the head is off unless asked for.
func TestChassisEnvGrantSocket(t *testing.T) {
	ws := t.TempDir()
	devDir := filepath.Join(ws, ".txco", "dev")
	env, _ := chassisEnv(chassisOpts{Workspace: ws, Heads: devHeads{Grant: true}, AllowLocalWorkspace: true},
		devAddrs, devDir, "", noParentEnv)
	if v, set := envOf(env)["TXCO_GRANT_SOCKET"]; set {
		t.Errorf("TXCO_GRANT_SOCKET = %q", v)
	}
	_, heads := chassisEnv(chassisOpts{Workspace: ws}, devAddrs, devDir, "", noParentEnv)
	if strings.Contains(strings.Join(heads, ","), "grant") {
		t.Errorf("heads with nothing asked for: %v", heads)
	}
}

func TestChassisEnvLocalWorkspace(t *testing.T) {
	ws := t.TempDir()
	devDir := filepath.Join(ws, ".txco", "dev")
	env, _ := chassisEnv(chassisOpts{Workspace: ws, AllowLocalWorkspace: true}, devAddrs, devDir, "", noParentEnv)
	got := envOf(env)
	for k, want := range map[string]string{
		"TXCO_WORKSPACE_PROVIDER":    "local",
		"TXCO_WORKSPACE_ALLOW_LOCAL": "true",
		"TXCO_WORKSPACE_LOCAL_ROOT":  filepath.Join(devDir, "workspaces"),
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	// It is this flag's alone: a parent that exported the opposite is
	// overridden, because the flag was given for this run.
	env, _ = chassisEnv(chassisOpts{Workspace: ws, AllowLocalWorkspace: true}, devAddrs, devDir, "",
		parentEnv(map[string]string{"TXCO_WORKSPACE_ALLOW_LOCAL": "false"}))
	if got := envOf(env)["TXCO_WORKSPACE_ALLOW_LOCAL"]; got != "true" {
		t.Errorf("with the flag, over a parent's false: %q", got)
	}
}
