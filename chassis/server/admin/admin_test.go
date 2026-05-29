package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	radix "github.com/hashicorp/go-immutable-radix"
	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// newTestController builds a Controller with an in-memory DB containing both
// `ops` and `op_revisions` tables, so the handlers can be exercised directly
// without starting an HTTP listener.
func newTestController(t *testing.T, conf config.Config) *Controller {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	// Production runs with MaxOpenConns=1 on the dbcache (sqlite is
	// single-writer anyway). Tests must match — a stock :memory: DB
	// is per-connection, so goroutines under MaxOpenConns>1 each see
	// a fresh empty database.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// Test scaffold: one :memory: DB plays both roles (runtime + auth).
	// Production runs two files; tests don't need that isolation — they
	// just need every table to exist and the same handle passed to both
	// the runtime and auth code paths.
	if _, err := db.Exec(runtimeSchemaSQL + authSchemaSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	logger := zap.NewNop()
	pu := &processor.Unit{
		Conf:      conf,
		Logger:    logger,
		RuntimeDB: db,
		AuthDB:    db,
		Dbc:       &dbcache.DbCache{Db: db, Logger: logger},
		Mux:       radix.New(),
	}
	c := &Controller{ctx: context.Background(), pu: pu}
	c.tenants = tenants.New(db)
	c.registry = registry.New(db, func(ctx context.Context, tenantID string) (string, error) {
		t, err := c.tenants.Lookup(ctx, tenantID)
		if err != nil {
			return "", err
		}
		return t.Slug, nil
	})
	c.nonces = auth.NewNonceStore(10 * time.Minute)
	c.verifier = signature.NewVerifier()
	// Mirror what Controller.Start does so devEnrollSecret /
	// devEnrollAutoGen pick up Conf.AuthDevEnrollSecret (and trigger
	// first-boot auto-generation when empty + registry empty).
	c.resolveDevEnrollSecret()
	return c
}

// withAdminCtx attaches a synthetic admin:all auth.Context to req, so
// direct-call handler tests (the ones that bypass the HTTP middleware)
// pass policy.RequireCapability. Mirrors what the middleware would do
// for a valid signed request.
func withAdminCtx(req *http.Request) *http.Request {
	c := &auth.Context{
		Source:       "signed",
		ActorID:      "actor_test",
		KeyID:        "key_test",
		Capabilities: []string{"admin:all"},
	}
	return req.WithContext(auth.WithContext(req.Context(), c))
}

// withTenantAdminCtx is like withAdminCtx but also stamps the tenant
// onto the auth.Context so handlers that read ac.TenantID (everything
// under /v1/tenants/{t}/) see a resolved tenant — same shape the
// resolveTenantMiddleware would have produced on a real request.
func withTenantAdminCtx(req *http.Request, tenantID string) *http.Request {
	c := &auth.Context{
		Source:       "signed",
		ActorID:      "actor_test",
		KeyID:        "key_test",
		Capabilities: []string{"admin:all"},
		TenantID:     tenantID,
		TenantSlug:   "default",
	}
	return req.WithContext(auth.WithContext(req.Context(), c))
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// (legacy TestImport* removed — the /ops/import endpoint is retired.
// Coverage for the write path now lives with the versioned control
// plane in chassis/server/admin/stacks_test.go: idempotency,
// multi-rule-per-scope, replace-updates-row, and parse-error rollback
// are exercised against POST /draft → PUT /files → POST /activate.)

// TestAuthMiddlewareBasicMode covers the basic-auth path through the new
// auth middleware (replaces the old c.basicAuth helper). When AdminUser
// is set, requests without (or with bad) credentials get 401; correct
// creds get a synthetic admin:all auth.Context.
func TestAuthMiddlewareBasicMode(t *testing.T) {
	conf := config.Config{
		Personalities: "admin",
		AuthMode:      "basic",
		AdminUser:     "alice",
		AdminPass:     "secret",
	}
	c := newTestController(t, conf)

	var seenCtx *auth.Context
	gated := auth.Middleware(auth.Config{
		Mode:      auth.AuthMode(conf.AuthMode),
		BasicUser: conf.AdminUser,
		BasicPass: conf.AdminPass,
		Registry:  c.registry,
		Verifier:  c.verifier,
		Nonces:    c.nonces,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenCtx = auth.FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	// No creds → 401.
	w1 := httptest.NewRecorder()
	gated.ServeHTTP(w1, httptest.NewRequest(http.MethodGet, "/v1/ops", nil))
	if w1.Code != http.StatusUnauthorized {
		t.Errorf("no-creds: got %d, want 401", w1.Code)
	}

	// Wrong password → 401.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/v1/ops", nil)
	r2.SetBasicAuth("alice", "wrong")
	gated.ServeHTTP(w2, r2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("wrong-pass: got %d, want 401", w2.Code)
	}

	// Correct creds → 200 with synthetic admin:all context.
	w3 := httptest.NewRecorder()
	r3 := httptest.NewRequest(http.MethodGet, "/v1/ops", nil)
	r3.SetBasicAuth("alice", "secret")
	gated.ServeHTTP(w3, r3)
	if w3.Code != http.StatusOK {
		t.Errorf("correct-creds: got %d, want 200", w3.Code)
	}
	if seenCtx == nil || seenCtx.Source != "basic" {
		t.Errorf("got ctx=%+v, want basic source", seenCtx)
	}
	if len(seenCtx.Capabilities) != 1 || seenCtx.Capabilities[0] != "admin:all" {
		t.Errorf("got capabilities=%v, want [admin:all]", seenCtx.Capabilities)
	}

	// Basic-auth context must NOT have created an actor row.
	var count int
	_ = c.pu.RuntimeDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM actors`).Scan(&count)
	if count != 0 {
		t.Errorf("basic-auth flow inserted %d actors, want 0 (kept out of actor model)", count)
	}
}

// TestAuthMiddlewareOpenDev verifies the legacy open-dev mode: when
// AdminUser is empty AND AuthMode is basic-or-both, the middleware
// lets requests through with an in-memory admin:all context.
func TestAuthMiddlewareOpenDev(t *testing.T) {
	conf := config.Config{Personalities: "admin", AuthMode: "basic"}
	c := newTestController(t, conf)

	gated := auth.Middleware(auth.Config{
		Mode:     auth.AuthMode(conf.AuthMode),
		Registry: c.registry,
		Verifier: c.verifier,
		Nonces:   c.nonces,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.FromContext(r.Context())
		if ctx == nil || ctx.Source != "open" {
			t.Errorf("got ctx=%+v, want open", ctx)
		}
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	gated.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/ops", nil))
	if w.Code != http.StatusOK {
		t.Errorf("no-auth-config: got %d, want 200 (open dev)", w.Code)
	}
}

// TestAuthMiddlewareSignedModeRejectsBasic confirms that signed mode
// refuses requests carrying only basic-auth, matching the user's
// expectation when migrating from `both` to `signed`.
func TestAuthMiddlewareSignedModeRejectsBasic(t *testing.T) {
	conf := config.Config{
		Personalities: "admin",
		AuthMode:      "signed",
		AdminUser:     "alice",
		AdminPass:     "secret",
	}
	c := newTestController(t, conf)

	gated := auth.Middleware(auth.Config{
		Mode:      auth.AuthMode(conf.AuthMode),
		BasicUser: conf.AdminUser,
		BasicPass: conf.AdminPass,
		Registry:  c.registry,
		Verifier:  c.verifier,
		Nonces:    c.nonces,
	}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/ops", nil)
	r.SetBasicAuth("alice", "secret")
	gated.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("signed-mode + basic creds: got %d, want 401", w.Code)
	}
}

// TestListOpsEmpty and TestListOpsWithFilter cover the GET /v1/ops handler's
// happy paths and the optional ?stack= prefix filter.
func TestListOpsEmpty(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	w := httptest.NewRecorder()
	c.handleListOps(w, withAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/ops", nil)))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d, body=%s", w.Code, w.Body.String())
	}
	var resp listOpsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Ops) != 0 {
		t.Errorf("got %d ops, want 0", len(resp.Ops))
	}
}

func TestListOpsWithFilter(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})

	// Seed `ops` directly — the versioned write path is exercised in
	// stacks_test.go; here we only need rows present so the read
	// endpoint can filter them by stack prefix.
	for _, row := range []struct {
		stack, name, txcl string
	}{
		{"website", "main", `EXEC "http://example.com/web"`},
		{"website/canary", "main", `EXEC "http://example.com/canary"`},
		{"support", "main", `EXEC "http://example.com/sup"`},
	} {
		if _, err := c.pu.RuntimeDB.Exec(
			`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res)
			 VALUES (?, ?, ?, ?, ?, '', '')`,
			"tnt_default", row.stack, 100, row.name, row.txcl); err != nil {
			t.Fatalf("seed %s: %v", row.stack, err)
		}
	}

	w2 := httptest.NewRecorder()
	c.handleListOps(w2, withAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/ops?stack=website", nil)))
	if w2.Code != http.StatusOK {
		t.Fatalf("filtered list: %d %s", w2.Code, w2.Body.String())
	}
	var resp listOpsResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Ops) != 2 {
		t.Errorf("got %d ops for ?stack=website, want 2 (website + website/canary)", len(resp.Ops))
	}
	for _, op := range resp.Ops {
		if op.Stack != "website" && op.Stack != "website/canary" {
			t.Errorf("unexpected stack in filter result: %s", op.Stack)
		}
	}
}
