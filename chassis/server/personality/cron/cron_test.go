package cron

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// TestCronTickFiresEvent verifies the cron personality posts an envelope to
// the bus on each tick with src="cron" and a well-formed payload, and that
// Stop() shuts the goroutine down cleanly.
//
// Uses CronPeriod=1 because the controller floors anything <= 0 to 1 second
// (see cron.go), so this is the fastest a real cron tick can be.
func TestCronTickFiresEvent(t *testing.T) {
	bus := make(chan *event.Envelope, 1)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities: "cron",
			CronPeriod:    1,
		},
		Logger: zap.NewNop(),
		Bus:    bus,
	}

	cc := NewController(context.Background(), pu)
	cc.Start()
	t.Cleanup(func() {
		// Best-effort stop in case the test fails before reaching Stop below.
		// Stop is idempotent enough at process exit.
		_ = cc
	})

	// First tick should land within ~1s; allow 3s for slow CI.
	select {
	case env := <-bus:
		if env.Src != "cron" {
			t.Errorf("envelope.Src = %q, want \"cron\"", env.Src)
		}
		if got := gjson.Get(env.Payload.Raw, "_txc.src").String(); got != "cron" {
			t.Errorf("payload _txc.src = %q, want \"cron\"", got)
		}
		if got := gjson.Get(env.Payload.Raw, "_ts").String(); got == "" {
			t.Errorf("payload _ts is empty, want RFC3339 timestamp")
		}
		// Unblock the cron tick goroutine which is waiting on resCh.
		env.ResCh <- event.Payload{Raw: `{}`, Type: event.JSON}
	case <-time.After(3 * time.Second):
		t.Fatal("no cron tick envelope on bus within 3s")
	}

	// Stop should return promptly and not hang.
	done := make(chan struct{})
	go func() {
		cc.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cron Stop() hung")
	}
}

// TestCronSubscribers verifies the opt-in: only non-revoked tenants
// that authored a `_cron` stack are returned, distinct, and a chassis
// with no dbcache (or no subscribers) yields none (off by default).
func TestCronSubscribers(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	mustExec := func(q string) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`)
	mustExec(`CREATE TABLE ops (tenant_id TEXT, stack TEXT, scope INT, name TEXT)`)
	mustExec(`INSERT INTO tenants VALUES ('tnt_acme','acme',NULL),('tnt_beta','beta',NULL),('tnt_gone','gone','2020-01-01T00:00:00Z')`)
	mustExec(`INSERT INTO ops VALUES
		('tnt_acme','_cron',100,'heartbeat'),
		('tnt_acme','_cron',0,'gate'),
		('tnt_acme','web',100,'hello'),
		('tnt_beta','web',100,'hello'),
		('tnt_gone','_cron',100,'heartbeat')`)

	cc := NewController(context.Background(), &processor.Unit{
		Conf:   config.Config{Personalities: "cron"},
		Logger: zap.NewNop(),
		Dbc:    &dbcache.DbCache{Db: db, Source: db},
	})

	got := cc.subscribers()
	if len(got) != 1 || got[0] != "acme" {
		t.Fatalf("subscribers = %v, want [acme] (beta has no _cron; gone is revoked; acme distinct)", got)
	}

	// No dbcache wired → no subscribers (off by default, no panic).
	ccNil := NewController(context.Background(), &processor.Unit{
		Conf: config.Config{Personalities: "cron"}, Logger: zap.NewNop(),
	})
	if s := ccNil.subscribers(); s != nil {
		t.Fatalf("nil dbcache subscribers = %v, want nil", s)
	}
}

// TestCronDisabledByPersonalityFlag verifies that omitting "cron" from the
// Personalities config makes Start() a no-op (no goroutine, no bus traffic).
// This is the documented opt-out and load-bearing for embedders who only
// want web/tcp inlets.
func TestCronDisabledByPersonalityFlag(t *testing.T) {
	bus := make(chan *event.Envelope, 1)
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities: "web,tcp", // no "cron"
			CronPeriod:    1,
		},
		Logger: zap.NewNop(),
		Bus:    bus,
	}

	cc := NewController(context.Background(), pu)
	cc.Start()

	// Wait long enough that a tick *would* have fired if the personality
	// were active, then assert the bus is still empty.
	select {
	case env := <-bus:
		t.Errorf("cron fired despite being disabled: got envelope %+v", env)
	case <-time.After(1500 * time.Millisecond):
		// Expected.
	}

	// Stop is also a no-op when disabled — verify it doesn't hang or panic.
	done := make(chan struct{})
	go func() {
		cc.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Stop() hung when cron was disabled")
	}
}
