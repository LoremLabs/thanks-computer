package lmtp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/mail"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

const ownDKIMSchema = `
CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT NOT NULL, revoked_at TEXT);
CREATE TABLE tenant_hostnames (id TEXT PRIMARY KEY, hostname TEXT NOT NULL, tenant_id TEXT NOT NULL, stack TEXT NOT NULL, created_at TEXT NOT NULL, created_by TEXT, revoked_at TEXT, verified_at TEXT, dkim_selector TEXT NOT NULL DEFAULT '', dkim_private_pem TEXT NOT NULL DEFAULT '', dkim_public_b64 TEXT NOT NULL DEFAULT '');
CREATE TABLE dns_zones (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, origin TEXT NOT NULL, mname TEXT, rname TEXT, refresh INTEGER, retry INTEGER, expire INTEGER, minimum INTEGER, default_ttl INTEGER, mode TEXT NOT NULL DEFAULT 'pattern', created_at TEXT NOT NULL, created_by TEXT, updated_at TEXT NOT NULL, revoked_at TEXT, verified_at TEXT, dkim_selector TEXT NOT NULL DEFAULT '', dkim_private_pem TEXT NOT NULL DEFAULT '', dkim_public_b64 TEXT NOT NULL DEFAULT '');`

const ownDKIMMessage = "From: Research Pony <researchpony@core-abc.stacks.example>\r\n" +
	"To: intern@core-abc.stacks.example\r\n" +
	"Subject: A question\r\n" +
	"Message-ID: <m1@core-abc.stacks.example>\r\n" +
	"Date: Sun, 04 Oct 2026 12:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"What does example.com say?\r\n"

// ownDKIMFixture is a mirror with two tenants: `onepony` owns a structured
// host with a per-host key and a delegated zone with its own key; `other`
// owns a host with a key of its own.
func ownDKIMFixture(t *testing.T) (db *sql.DB, hostKey, zoneKey, otherKey string) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(ownDKIMSchema); err != nil {
		t.Fatal(err)
	}
	gen := func() (priv, pub string) {
		priv, pub, err := tenants.GenerateDKIM()
		if err != nil {
			t.Fatal(err)
		}
		return priv, pub
	}
	hostKey, hostPub := gen()
	zoneKey, _ = gen() // stored WITHOUT its public half: the lookup derives it
	otherKey, otherPub := gen()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tenants (tenant_id, slug, created_at) VALUES ('tnt_1', 'onepony', 'now'), ('tnt_2', 'other', 'now')`, nil},
		{`INSERT INTO tenant_hostnames (id, hostname, tenant_id, stack, created_at, verified_at, dkim_selector, dkim_private_pem, dkim_public_b64)
		  VALUES ('h1', 'core-abc.stacks.example', 'tnt_1', 'core', 'now', 'now', 'txco', ?, ?)`, []any{hostKey, hostPub}},
		{`INSERT INTO tenant_hostnames (id, hostname, tenant_id, stack, created_at, verified_at, dkim_selector, dkim_private_pem, dkim_public_b64)
		  VALUES ('h2', 'core-xyz.stacks.example', 'tnt_2', 'core', 'now', 'now', 'txco', ?, ?)`, []any{otherKey, otherPub}},
		{`INSERT INTO dns_zones (id, tenant_id, origin, created_at, updated_at, verified_at, dkim_selector, dkim_private_pem)
		  VALUES ('z1', 'tnt_1', 'ponies.example', 'now', 'now', 'now', 'txco', ?)`, []any{zoneKey}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return db, hostKey, zoneKey, otherKey
}

// signAs signs msg exactly as chassis/mail dkimSign does.
func signAs(t *testing.T, msg, domain, selector, privPEM string) []byte {
	t.Helper()
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil {
		t.Fatal("no PEM block")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := dkim.Sign(&buf, strings.NewReader(msg), &dkim.SignOptions{
		Domain:                 domain,
		Selector:               selector,
		Signer:                 crypto.Signer((*rsa.PrivateKey)(key)),
		Hash:                   crypto.SHA256,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
	}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestOwnDKIM(t *testing.T) {
	db, hostKey, zoneKey, otherKey := ownDKIMFixture(t)
	ctx := context.Background()
	const host, zone, otherHost = "core-abc.stacks.example", "ponies.example", "core-xyz.stacks.example"

	signed := signAs(t, ownDKIMMessage, host, "txco", hostKey)
	// What a relay does on the way: headers on top, none of them signed.
	relayed := append([]byte("Received: from edge by mx; Sun, 04 Oct 2026 12:00:01 +0000\r\nX-Spamd-Result: default: False [0.10 / 15.00]\r\n"), signed...)
	zoneMsg := strings.ReplaceAll(ownDKIMMessage, "@core-abc.stacks.example", "@ponies.example")
	foreign, _, err := tenants.GenerateDKIM()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, tenant string
		msg          []byte
		want, wantD  string
	}{
		{"the tenant's own host signed it", "onepony", signed, "pass", host},
		{"…and it survives the relay's added headers", "onepony", relayed, "pass", host},
		{"the tenant's own zone signed it (public half derived)", "onepony", signAs(t, zoneMsg, zone, "txco", zoneKey), "pass", zone},
		{"the body was changed", "onepony", bytes.Replace(signed, []byte("example.com say"), []byte("example.org say"), 1), "fail", ""},
		{"the From was changed", "onepony", bytes.Replace(signed, []byte("From: Research Pony <researchpony@"), []byte("From: Research Pony <intern@"), 1), "fail", ""},
		{"the tenant's domain, a key that is not the tenant's", "onepony", signAs(t, ownDKIMMessage, host, "txco", foreign), "fail", ""},
		{"the tenant's domain, a selector it has no key for", "onepony", signAs(t, ownDKIMMessage, host, "other", hostKey), "fail", ""},
		{"no signature", "onepony", []byte(ownDKIMMessage), "none", ""},
		{"a foreign domain's signature", "onepony", signAs(t, ownDKIMMessage, "elsewhere.example", "txco", foreign), "none", ""},
		{"another tenant's host, validly signed, is not this tenant's", "onepony", signAs(t, ownDKIMMessage, otherHost, "txco", otherKey), "none", ""},
		{"…and is that tenant's own", "other", signAs(t, ownDKIMMessage, otherHost, "txco", otherKey), "pass", otherHost},
		{"this tenant's signature proves nothing to another tenant", "other", signed, "none", ""},
		{"no tenant", "", signed, "none", ""},
	}
	for _, c := range cases {
		got, d := ownDKIM(ctx, db, registry.SQLite, c.tenant, c.msg)
		if got != c.want || d != c.wantD {
			t.Errorf("%s: got %s %q, want %s %q", c.name, got, d, c.want, c.wantD)
		}
	}

	if got, d := ownDKIM(ctx, nil, registry.SQLite, "onepony", signed); got != "none" || d != "" {
		t.Errorf("no store: got %s %q, want none", got, d)
	}
}

// The head finds a signature through the parsed message's headers: the
// parser must keep DKIM-Signature under the key the gate reads.
func TestOwnDKIMGateReadsTheParsedHeader(t *testing.T) {
	_, hostKey, _, _ := ownDKIMFixture(t)
	signed := signAs(t, ownDKIMMessage, "core-abc.stacks.example", "txco", hostKey)
	msgJSON, err := mail.ParseMessage(signed)
	if err != nil {
		t.Fatal(err)
	}
	if firstHeader(msgJSON, "dkim-signature") == "" {
		t.Fatal("a signed message parses with no dkim-signature header: the head would never verify it")
	}
	plain, _ := mail.ParseMessage([]byte(ownDKIMMessage))
	if firstHeader(plain, "dkim-signature") != "" {
		t.Fatal("an unsigned message parses with a dkim-signature header")
	}
}
