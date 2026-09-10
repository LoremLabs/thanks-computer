package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	src "github.com/loremlabs/thanks-computer/chassis/source"
)

// ---- a scripted fake source kind -----------------------------------------

func init() { src.RegisterKind(fakeKind{}) }

// lastConn exposes the most recently opened fake connection so a test can read
// back what the poller acked (the kind registry gives no other handle).
var (
	lastConnMu sync.Mutex
	lastConn   *fakeConn
)

type fakeKind struct{}

func (fakeKind) Name() string { return "faketest" }

type fakeItemSpec struct {
	Key string `json:"key"`
	Cur string `json:"cur"`
}

func (fakeKind) Open(ctx context.Context, p src.OpenParams) (src.Conn, error) {
	var cfg struct {
		Items []fakeItemSpec `json:"items"`
	}
	if err := json.Unmarshal(p.Config, &cfg); err != nil {
		return nil, err
	}
	items := make([]src.Item, len(cfg.Items))
	for i, it := range cfg.Items {
		items[i] = src.Item{
			Key:    it.Key,
			Raw:    []byte("Subject: t\r\n\r\nbody\r\n"),
			Meta:   json.RawMessage(`{"uid":` + strconv.Itoa(i+1) + `}`),
			Cursor: src.Cursor(`"` + it.Cur + `"`),
		}
	}
	conn := &fakeConn{items: items, acked: map[string]src.Action{}}
	lastConnMu.Lock()
	lastConn = conn
	lastConnMu.Unlock()
	return conn, nil
}

type fakeConn struct {
	items []src.Item
	mu    sync.Mutex
	acked map[string]src.Action
}

func (c *fakeConn) Poll(ctx context.Context, cur src.Cursor, limit int) ([]src.Item, src.Cursor, error) {
	return c.items, nil, nil
}
func (c *fakeConn) Ack(ctx context.Context, it src.Item, act src.Action) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acked[it.Key] = act
	return nil
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) ackOf(key string) src.Action {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acked[key]
}

// ---- harness -------------------------------------------------------------

// busResponder replies to each dispatched envelope. deny is the item key whose
// run is admission-denied (empty = none); override maps an item key to an
// action the "stack" proposes back via _txc.source.res.action.
func busResponder(ctx context.Context, bus chan *event.Envelope, deny string, override map[string]string) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-bus:
				key := gjson.Get(env.Payload.Raw, "_txc.source.key").String()
				resp := `{}`
				if key == deny {
					resp = `{"_txc":{"admission":{"denied":true,"status":402}}}`
				} else if act, ok := override[key]; ok {
					resp = `{"_txc":{"source":{"res":{"action":"` + act + `"}}}}`
				}
				env.ResCh <- event.Payload{Raw: resp}
			}
		}
	}()
}

func newPoller(t *testing.T) (*Controller, *src.Store, chan *event.Envelope) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := src.NewStore(db, registry.SQLite)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("schema: %v", err)
	}
	conf := config.Config{Personalities: "source", SourceBatch: 50, SourceDispatchTimeout: 5}
	bus := make(chan *event.Envelope)
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Bus: bus}
	c := NewController(context.Background(), pu, store, nil)
	return c, store, bus
}

func fakeSource(t *testing.T, store *src.Store, items ...fakeItemSpec) src.Claimed {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{
		"id": "s", "kind": "faketest", "on_processed": "move:Done", "items": items,
	})
	if err := store.Upsert(context.Background(), src.Declared{
		Tenant: "acme", Stack: "desk", Pack: "boxes", DeclaredID: "s",
		Kind: "faketest", Config: cfg, Enabled: true, EverySeconds: 300, Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	won, err := store.ClaimDue(context.Background(), "node", 10)
	if err != nil || len(won) != 1 {
		t.Fatalf("claim: won=%d err=%v", len(won), err)
	}
	return won[0]
}

func TestPollerAdvancesPerSuccessAndAcks(t *testing.T) {
	c, store, bus := newPoller(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	busResponder(ctx, bus, "", nil)

	cl := fakeSource(t, store,
		fakeItemSpec{Key: "v:1", Cur: "c1"},
		fakeItemSpec{Key: "v:2", Cur: "c2"},
		fakeItemSpec{Key: "v:3", Cur: "c3"})

	c.handleSource(ctx, cl)

	sts, _ := store.List(ctx, "acme")
	if sts[0].Cursor != `"c3"` {
		t.Errorf("cursor = %q, want c3", sts[0].Cursor)
	}
	if sts[0].LastError != "" {
		t.Errorf("unexpected last_error: %q", sts[0].LastError)
	}
	// All three acked with the configured default action.
	for _, k := range []string{"v:1", "v:2", "v:3"} {
		if a := lastConn.ackOf(k); a.Op != "move" || a.Dest != "Done" {
			t.Errorf("ack[%s] = %+v, want move:Done", k, a)
		}
	}
}

func TestPollerStopsAtFirstFailure(t *testing.T) {
	c, store, bus := newPoller(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	busResponder(ctx, bus, "v:2", nil) // item 2's run is denied

	cl := fakeSource(t, store,
		fakeItemSpec{Key: "v:1", Cur: "c1"},
		fakeItemSpec{Key: "v:2", Cur: "c2"},
		fakeItemSpec{Key: "v:3", Cur: "c3"})

	c.handleSource(ctx, cl)

	sts, _ := store.List(ctx, "acme")
	if sts[0].Cursor != `"c1"` {
		t.Errorf("cursor = %q, want c1 (stop at the denied item)", sts[0].Cursor)
	}
	if sts[0].LastError == "" {
		t.Errorf("expected last_error on a partial batch")
	}
	// v:1 acked, v:2 and v:3 never acked (batch stopped).
	if lastConn.ackOf("v:1").Op != "move" {
		t.Errorf("v:1 should have been acked")
	}
	if lastConn.ackOf("v:2").Op != "" || lastConn.ackOf("v:3").Op != "" {
		t.Errorf("v:2/v:3 must not be acked after the failure")
	}
}

func TestPollerHonorsStackActionOverride(t *testing.T) {
	c, store, bus := newPoller(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	busResponder(ctx, bus, "", map[string]string{"v:1": "seen"})

	cl := fakeSource(t, store, fakeItemSpec{Key: "v:1", Cur: "c1"})
	c.handleSource(ctx, cl)

	// The stack's override wins over the configured move:Done.
	if a := lastConn.ackOf("v:1"); a.Op != "seen" {
		t.Errorf("ack = %+v, want the overridden 'seen'", a)
	}
	sts, _ := store.List(ctx, "acme")
	if sts[0].Cursor != `"c1"` || sts[0].LastError != "" {
		t.Errorf("override run should complete cleanly: %+v", sts[0])
	}
}
