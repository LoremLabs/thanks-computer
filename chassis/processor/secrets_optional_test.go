package processor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/ops"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// TestSecretsOptionalNotFound pins the `secrets.<x>.optional = true`
// contract at the processor: with the store wired and the tenant seeded
// but NO secret of that name, an op that declares the reference optional
// still RUNS (its handler sees the name absent from the bag), while the
// same op without the flag is skipped exactly as before. The op under
// test is txco://basic-auth-verify, whose output says which case it saw.
func TestSecretsOptionalNotFound(t *testing.T) {
	pu, _ := newTestUnit(t)
	if _, err := pu.Dbc.Db.Exec(secretStoreSchema); err != nil {
		t.Fatalf("create secret store tables: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "master.key")
	if err := secrets.MintFileMasterKey(keyPath); err != nil {
		t.Fatalf("mint master key: %v", err)
	}
	mk, err := secrets.NewFileMasterKey(keyPath)
	if err != nil {
		t.Fatalf("load master key: %v", err)
	}
	store := secrets.NewStore(pu.Dbc.Db, mk)
	slugToID := func(ctx context.Context, slug string) (string, error) {
		var id string
		return id, pu.Dbc.Db.QueryRowContext(ctx,
			`SELECT tenant_id FROM tenants WHERE slug = ? AND revoked_at IS NULL`, slug,
		).Scan(&id)
	}
	pu.Secrets = secrets.NewResolver(store, slugToID)
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_acme', 'acme', '2026-05-20T00:00:00Z')`,
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	pu.Handle([]byte("txco://basic-auth-verify"), event.OpsHandlerFunc(ops.BasicAuthVerify))
	logCore, logs := observer.New(zap.DebugLevel)
	pu.Logger = zap.New(logCore)
	t.Cleanup(func() {
		if t.Failed() {
			for _, e := range logs.All() {
				t.Logf("log: %s %s %v", e.Level, e.Message, e.ContextMap())
			}
		}
	})

	seed := func(stack, rule string) {
		t.Helper()
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES (?, ?, ?, ?, '', '', 'tnt_acme')`,
			stack, 100, "gate", rule,
		); err != nil {
			t.Fatalf("seed op: %v", err)
		}
	}
	run := func(stack string) string {
		t.Helper()
		resCh := make(chan event.Payload, 1)
		if err := pu.Run(context.Background(), `{"trigger":"fire","_txc":{"tenant":"acme"}}`, stack+"/100", resCh); err != nil {
			t.Fatalf("Run %s: %v", stack, err)
		}
		select {
		case p := <-resCh:
			return p.Raw
		default:
			t.Fatalf("no response from Run %s", stack)
			return ""
		}
	}

	// Optional + allow_unconfigured: the op runs and reports "not configured".
	seed("e2e/opt", `WHEN .trigger == "fire" EXEC "txco://basic-auth-verify" `+
		`WITH user = "provision", secrets.password.secret = "NOT_SET", `+
		`secrets.password.optional = true, allow_unconfigured = true`)
	out := run("e2e/opt")
	if c := gjson.Get(out, "_txc.computed.basic_auth_configured"); !c.Exists() || c.Bool() {
		t.Errorf("optional: basic_auth_configured = %v, want false (op ran, secret absent); out=%s", c, out)
	}
	if !gjson.Get(out, "_txc.computed.basic_auth_ok").Bool() {
		t.Errorf("optional + allow_unconfigured: basic_auth_ok = false, want true; out=%s", out)
	}

	// Optional without allow_unconfigured: runs, fails closed.
	seed("e2e/opt-closed", `WHEN .trigger == "fire" EXEC "txco://basic-auth-verify" `+
		`WITH user = "provision", secrets.password.secret = "NOT_SET", secrets.password.optional = true`)
	out = run("e2e/opt-closed")
	if ok := gjson.Get(out, "_txc.computed.basic_auth_ok"); !ok.Exists() || ok.Bool() {
		t.Errorf("optional, default: basic_auth_ok = %v, want explicit false; out=%s", ok, out)
	}

	// Required (no flag): the dispatch fails and the op never runs.
	seed("e2e/req", `WHEN .trigger == "fire" EXEC "txco://basic-auth-verify" `+
		`WITH user = "provision", secrets.password.secret = "NOT_SET"`)
	out = run("e2e/req")
	if gjson.Get(out, "_txc.computed.basic_auth_ok").Exists() {
		t.Errorf("required secret missing but the op ran: %s", out)
	}
}
