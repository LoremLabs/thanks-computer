package processor

// Continuable-as-op-metadata acceptance tests (docs/todo-continuable-metadata):
// `mode = "continuable"` is honored on ANY transport, driven by the WITH
// metadata alone. The interesting properties are not the gate but the
// plumbing the gate exposed:
//
//   - ai://chat runs continuable: fast completions stay fully synchronous
//     (chat stamps inline), slow ones promote and the `_txc.chat.*` stamps
//     SURVIVE the resume merge (trust bit persisted on the terminal).
//   - The detached context carries the tenant pin, so per-tenant ai://
//     secrets resolve without the env fallback.
//   - WITH `secrets.*` materializes on the continuable path (it never did
//     before this change).
//   - Detached work charges fuel (terminal FuelUsed → folded in at resume).
//   - A continuable promoting INSIDE a resumed pipeline reuses the run and
//     never emits a 202 into the resume capture channel.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// slowChatBackend is a registerable chat backend whose Run sleeps `delay`
// before answering — used to dial the completion across the continue_after
// deadline. Captures the first required secret's cleartext it saw.
type slowChatBackend struct {
	name     string
	delay    time.Duration
	required []string
	resp     chat.Response

	mu     sync.Mutex
	sawKey string
}

func (b *slowChatBackend) Name() string              { return b.name }
func (b *slowChatBackend) Capabilities() []string    { return []string{"public_execution"} }
func (b *slowChatBackend) RequiredSecrets() []string { return b.required }

func (b *slowChatBackend) Run(ctx context.Context, req chat.Request, bag *secrets.SecretBag) (chat.Response, error) {
	if len(b.required) > 0 {
		if v, ok := bag.Get(b.required[0]); ok {
			b.mu.Lock()
			b.sawKey = string(v)
			b.mu.Unlock()
		}
	}
	if b.delay > 0 {
		select {
		case <-time.After(b.delay):
		case <-ctx.Done():
			return chat.Response{}, ctx.Err()
		}
	}
	return b.resp, nil
}

func (b *slowChatBackend) key() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sawKey
}

func registerSlowBackend(t *testing.T, b *slowChatBackend) {
	t.Helper()
	chat.Register(b.name, func(cfg chat.Config) (chat.Backend, error) { return b, nil })
}

func newContinuableUnit(t *testing.T) *Unit {
	t.Helper()
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	return pu
}

// setupTenantSecretStore wires a real secret store + resolver onto pu, seeds
// tenant acme, and stores one secret. Mirrors TestSecretsEndToEnd's setup.
func setupTenantSecretStore(t *testing.T, pu *Unit, secretName, cleartext string) {
	t.Helper()
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
	if _, err := store.CreateSecret(
		context.Background(), "tnt_acme", nil, secretName, "", "actor_test", []byte(cleartext),
	); err != nil {
		t.Fatalf("CreateSecret: %v", err)
	}
}

func seedTenantOp(t *testing.T, pu *Unit, stack string, scope int, name, rule string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES (?, ?, ?, ?, '', '', 'tnt_acme')`,
		stack, scope, name, rule); err != nil {
		t.Fatalf("seed %s/%d: %v", stack, scope, err)
	}
}

// --- 1. ai://chat, fast: stays fully synchronous, chat stamps inline -------

func TestContinuableAIChatFastStaysSync(t *testing.T) {
	pu := newContinuableUnit(t)
	stub := &slowChatBackend{
		name: "tb-cont-fast",
		resp: chat.Response{Text: "quick answer", Provider: "tb-cont-fast", Model: "m1", TokensIn: 7, TokensOut: 3},
	}
	registerSlowBackend(t, stub)

	seedOp(t, pu, "acme", 100, "decide",
		`EXEC "ai://chat" WITH mode = "continuable", continue_after = "1s", timeout = "5s", provider = "tb-cont-fast", prompt = "hi"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	select {
	case p := <-resCh:
		if got := gjson.Get(p.Raw, "_txc.web.res.status").Int(); got == 202 {
			t.Fatalf("fast ai chat promoted; want sync inline: %s", p.Raw)
		}
		if got := gjson.Get(p.Raw, "text").String(); got != "quick answer" {
			t.Errorf("text = %q, want %q (envelope %s)", got, "quick answer", p.Raw)
		}
		if got := gjson.Get(p.Raw, "_txc.chat.provider").String(); got != "tb-cont-fast" {
			t.Errorf("_txc.chat.provider = %q, want tb-cont-fast (trusted stamps must ride sync)", got)
		}
		// Fuel the op drew on its detached budget transfers to the request
		// meter on the sync-win branch: scope-enter (10) + exec (25) at least.
		if got := gjson.Get(p.Raw, "_txc.fuel_used").Int(); got < 35 {
			t.Errorf("_txc.fuel_used = %d, want >= 35 (sync-win fuel transfer)", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no sync response within 3s")
	}
}

// --- 2. ai://chat, slow: promotes; chat stamps survive the resume merge ----

func TestContinuableAIChatPromotesAndStampsSurvive(t *testing.T) {
	pu := newContinuableUnit(t)
	stub := &slowChatBackend{
		name:  "tb-cont-slow",
		delay: 400 * time.Millisecond,
		resp:  chat.Response{Text: "late answer", Provider: "tb-cont-slow", Model: "m2", TokensIn: 11, TokensOut: 4},
	}
	registerSlowBackend(t, stub)

	seedOp(t, pu, "acme", 100, "decide",
		`EXEC "ai://chat" WITH mode = "continuable", continue_after = "100ms", timeout = "5s", provider = "tb-cont-slow", prompt = "hi"`)
	seedOp(t, pu, "acme", 200, "render", `EMIT .resumed = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("post-resume state = %q, want completed", st)
	}

	ctx := context.Background()
	res, ok, _ := pu.Runs.ReadResult(ctx, runID)
	if !ok {
		t.Fatal("no result.json after resume")
	}
	// The acceptance line: `_txc.chat.*` survives on the merged output.
	if got := gjson.GetBytes(res, "_txc.chat.provider").String(); got != "tb-cont-slow" {
		t.Errorf("_txc.chat.provider missing from resumed result (trust bit lost?); result=%s", res)
	}
	if got := gjson.GetBytes(res, "text").String(); got != "late answer" {
		t.Errorf("text = %q, want %q", got, "late answer")
	}
	if !gjson.GetBytes(res, "resumed").Bool() {
		t.Errorf("downstream scope did not run post-resume; result=%s", res)
	}

	// Terminal provenance: transport "ai", detached fuel recorded (>= the
	// 25-fuel exec charge), and the resumed envelope's meter includes it.
	term, err := pu.Runs.ReadOpTerminal(ctx, runID, "acme/100", 0, "decide")
	if err != nil {
		t.Fatalf("ReadOpTerminal: %v", err)
	}
	if term.Transport != "ai" {
		t.Errorf("terminal transport = %q, want ai", term.Transport)
	}
	if term.FuelUsed < 25 {
		t.Errorf("terminal fuel_used = %d, want >= 25 (detached exec charge)", term.FuelUsed)
	}
	ss, err := pu.Runs.ReadStageSuspended(ctx, runID, "acme/100")
	if err != nil {
		t.Fatalf("ReadStageSuspended: %v", err)
	}
	suspendFuel := FuelUsedFromEnvelope(ss.ScopeEnvelope)
	if got := gjson.GetBytes(res, "_txc.fuel_used").Int(); got < suspendFuel+term.FuelUsed {
		t.Errorf("result fuel = %d, want >= suspend(%d) + detached(%d)", got, suspendFuel, term.FuelUsed)
	}
}

// --- 3. terminal projection: trust keyed on the persisted transport --------

func TestSanitizeTerminalOutputByTransport(t *testing.T) {
	in := `{"answer":42,"_txc":{"chat":{"provider":"p","tokens":{"in":1}},"computed":{"sig_valid":true},"tenant":"intruder","fuel_used":9999,"ttl":5,"web":{"res":{"status":200}}}}`
	cases := []struct {
		transport string
		wantChat  bool
	}{
		{"ai", true},     // trusted: chat + computed survive
		{"txco", true},   // trusted
		{"", false},      // worker callback / legacy doc: fail closed
		{"https", false}, // author-controlled
		{"mock", false},
	}
	for _, c := range cases {
		out := sanitizeTerminalOutput(c.transport, in)
		if got := gjson.Get(out, "_txc.chat.provider").Exists(); got != c.wantChat {
			t.Errorf("transport %q: chat survived = %v, want %v (out=%s)", c.transport, got, c.wantChat, out)
		}
		if got := gjson.Get(out, "_txc.computed.sig_valid").Exists(); got != c.wantChat {
			t.Errorf("transport %q: computed survived = %v, want %v", c.transport, got, c.wantChat)
		}
		// The unforgeable core NEVER survives the store, trusted or not.
		if gjson.Get(out, "_txc.tenant").Exists() {
			t.Errorf("transport %q: _txc.tenant escaped the projection: %s", c.transport, out)
		}
		if gjson.Get(out, "_txc.fuel_used").Exists() || gjson.Get(out, "_txc.ttl").Exists() {
			t.Errorf("transport %q: budget fields escaped the projection: %s", c.transport, out)
		}
		// Author-writable paths survive for everyone.
		if !gjson.Get(out, "_txc.web.res.status").Exists() {
			t.Errorf("transport %q: allowlisted web.res dropped: %s", c.transport, out)
		}
		if gjson.Get(out, "answer").Int() != 42 {
			t.Errorf("transport %q: non-_txc data mangled: %s", c.transport, out)
		}
	}
}

// --- 4. WITH secrets.* materializes on the continuable path ---------------

func TestContinuableSecretsMaterializeThroughPromotion(t *testing.T) {
	pu := newContinuableUnit(t)
	const cleartext = "sk_live_continuable_xyz"
	setupTenantSecretStore(t, pu, "STRIPE_API_KEY", cleartext)

	var (
		mu      sync.Mutex
		gotAuth string
	)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		select { // answer AFTER continue_after so the op promotes
		case <-time.After(400 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"charged":true}`))
	}))
	t.Cleanup(mock.Close)

	seedTenantOp(t, pu, "e2e/csec", 100, "charge",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "5s", `+
			`secrets.headers.authorization.secret = "STRIPE_API_KEY", `+
			`secrets.headers.authorization.format = "Bearer {}"`, mock.URL))

	resCh := make(chan event.Payload, 1)
	go func() {
		_ = pu.Run(context.Background(), `{"_txc":{"tenant":"acme"}}`, "e2e/csec/100", resCh)
	}()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("post-resume state = %q, want completed", st)
	}

	mu.Lock()
	auth := gotAuth
	mu.Unlock()
	if auth != "Bearer "+cleartext {
		t.Errorf("upstream Authorization = %q, want %q (WITH secrets never materialized on the continuable path?)",
			auth, "Bearer "+cleartext)
	}
	res, ok, _ := pu.Runs.ReadResult(context.Background(), runID)
	if !ok || !gjson.GetBytes(res, "charged").Bool() {
		t.Errorf("resumed result missing upstream body: %s", res)
	}
}

// --- 5. detached ctx carries the tenant pin: per-tenant ai:// secrets ------

func TestContinuableAIChatResolvesTenantSecretDetached(t *testing.T) {
	pu := newContinuableUnit(t)
	pu.Conf.AIChatEnvFallback = false // isolation mode: env fallback would mask the regression
	const cleartext = "or-key-tenant-scoped"
	setupTenantSecretStore(t, pu, "OPENROUTER_KEY", cleartext)

	stub := &slowChatBackend{
		name:     "tb-cont-tenant",
		delay:    400 * time.Millisecond,
		required: []string{"OPENROUTER_KEY"},
		resp:     chat.Response{Text: "tenant answer", Provider: "tb-cont-tenant"},
	}
	registerSlowBackend(t, stub)

	seedTenantOp(t, pu, "e2e/ct", 100, "decide",
		`EXEC "ai://chat" WITH mode = "continuable", continue_after = "100ms", timeout = "5s", provider = "tb-cont-tenant", prompt = "hi"`)

	resCh := make(chan event.Payload, 1)
	go func() {
		_ = pu.Run(context.Background(), `{"_txc":{"tenant":"acme"}}`, "e2e/ct/100", resCh)
	}()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("post-resume state = %q, want completed", st)
	}
	if got := stub.key(); got != cleartext {
		t.Errorf("backend saw key %q, want tenant-store cleartext %q (tenant pin lost on the detached ctx?)",
			got, cleartext)
	}
}

// --- 6. continuable under resume: reuse the run, no 202 into capCh ---------

func TestContinuableUnderResumeDoesNotClobberResult(t *testing.T) {
	pu := newContinuableUnit(t)

	stubA, _ := delayedStub(t, 400*time.Millisecond, `{"first":"a"}`)
	stubB, _ := delayedStub(t, 400*time.Millisecond, `{"second":"b"}`)

	seedOp(t, pu, "acme", 100, "one",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "5s"`, stubA.URL))
	seedOp(t, pu, "acme", 200, "two",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "5s"`, stubB.URL))
	seedOp(t, pu, "acme", 300, "done", `EMIT .done = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	// Exactly one 202 — the second promotion happens inside the resumed
	// pipeline where no client is attached.
	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("final state = %q, want completed", st)
	}

	res, ok, _ := pu.Runs.ReadResult(context.Background(), runID)
	if !ok {
		t.Fatal("no result.json")
	}
	// The stored result must be the FINAL envelope, not a 202 wait page a
	// mid-resume promotion pushed into the capture channel.
	if got := gjson.GetBytes(res, "_txc.web.res.status").Int(); got == 202 {
		t.Fatalf("result.json is a 202 wait envelope (re-entrancy guard missing): %s", res)
	}
	for _, field := range []string{"first", "second", "done"} {
		if !gjson.GetBytes(res, field).Exists() {
			t.Errorf("result missing %q — full pipeline did not run: %s", field, res)
		}
	}
}
