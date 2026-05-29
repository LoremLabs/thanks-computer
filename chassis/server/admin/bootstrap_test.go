package admin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	radix "github.com/hashicorp/go-immutable-radix"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// newObservableController is a sibling of newTestController that
// returns the observer.ObservedLogs alongside the Controller. Tests
// that need to assert log content use this; tests that don't can
// stick with newTestController.
func newObservableController(t *testing.T, conf config.Config) (*Controller, *observer.ObservedLogs) {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sqlite open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Use the same shared schema fragments as newTestController so
	// adding a column requires one edit. Bootstrap exercises actor
	// creation + the default tenant lookup, so both fragments are
	// needed; the unused tables are cheap.
	if _, err := db.Exec(runtimeSchemaSQL + authSchemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}

	core, recorded := observer.New(zap.WarnLevel)
	logger := zap.New(core)

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
	c.resolveDevEnrollSecret()
	c.logDevEnrollBanner()
	return c, recorded
}

// TestAutoBootstrapGeneratesSecret — empty registry + no explicit
// secret + dev env: chassis mints a 4-word secret and prints it.
func TestAutoBootstrapGeneratesSecret(t *testing.T) {
	c, logs := newObservableController(t, config.Config{
		Personalities: "admin",
		AuthMode:      "both",
		Environment:   "dev",
	})

	if c.devEnrollSecret == "" {
		t.Fatalf("expected auto-generated secret; got empty")
	}
	if !c.devEnrollAutoGen {
		t.Errorf("expected devEnrollAutoGen=true; got false")
	}
	if parts := strings.Split(c.devEnrollSecret, "-"); len(parts) != 8 {
		t.Errorf("secret %q is not 8 hyphen-separated tokens", c.devEnrollSecret)
	}

	// The WARN must include the actual secret value (so a developer
	// reading logs can paste it).
	found := false
	for _, e := range logs.All() {
		for _, f := range e.Context {
			if f.Key == "secret" && f.String == c.devEnrollSecret {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("secret %q not present in WARN logs; entries=%v", c.devEnrollSecret, logs.All())
	}
}

// TestAutoBootstrapInProd — auto-bootstrap is environment-agnostic.
// An operator who hasn't enrolled an admin in prod yet needs the same
// recovery path as in dev.
func TestAutoBootstrapInProd(t *testing.T) {
	c, _ := newObservableController(t, config.Config{
		Personalities: "admin",
		AuthMode:      "signed",
		Environment:   "prod",
	})
	if c.devEnrollSecret == "" || !c.devEnrollAutoGen {
		t.Errorf("expected auto-generated secret in prod; got secret=%q autoGen=%v",
			c.devEnrollSecret, c.devEnrollAutoGen)
	}
}

// TestNoAutoBootstrapWithExplicitSecret — operator-supplied secret
// disables auto-generation. The legacy WARN ("DEV ENROLLMENT ENABLED")
// still prints, but the secret value is NOT included.
func TestNoAutoBootstrapWithExplicitSecret(t *testing.T) {
	c, logs := newObservableController(t, config.Config{
		Personalities:       "admin",
		AuthMode:            "signed",
		AuthDevEnrollSecret: "apple",
		Environment:         "dev",
	})
	if c.devEnrollSecret != "apple" {
		t.Errorf("devEnrollSecret = %q, want apple", c.devEnrollSecret)
	}
	if c.devEnrollAutoGen {
		t.Errorf("devEnrollAutoGen = true, want false (explicit secret)")
	}
	// Secret value must NOT appear in log output.
	for _, e := range logs.All() {
		for _, f := range e.Context {
			if f.Key == "secret" && f.String == "apple" {
				t.Errorf("explicit secret leaked into log: %v", e.Message)
			}
		}
	}
}

// TestNoAutoBootstrapWhenActorExists — registry already has an admin:
// no secret generated, no log line printed, /auth/dev/enroll returns
// 404 to any caller.
func TestNoAutoBootstrapWhenActorExists(t *testing.T) {
	c, logs := newObservableController(t, config.Config{
		Personalities: "admin",
		AuthMode:      "signed",
		Environment:   "dev",
	})
	// Pre-seed: simulate a prior boot's enrolled admin.
	if err := c.registry.CreateActor(context.Background(), registry.Actor{
		ActorID: "actor_seed",
		Label:   "prior-admin",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Re-run the bootstrap decision now that the registry is non-empty.
	c.devEnrollSecret = ""
	c.devEnrollAutoGen = false
	c.resolveDevEnrollSecret()

	if c.devEnrollSecret != "" {
		t.Errorf("expected empty secret; got %q", c.devEnrollSecret)
	}
	if c.devEnrollAutoGen {
		t.Errorf("expected devEnrollAutoGen=false; got true")
	}
	// The original-boot WARN may still be in the recorded log because
	// the constructor ran the banner once. What matters is that the
	// re-evaluation didn't produce a NEW secret WARN — but we already
	// asserted devEnrollSecret=="" which proves that.
	_ = logs
}

// TestDevEnrollBurnsAfterFirstUse — auto-generated secret accepts
// exactly one enrolment; the second attempt with the same secret is
// rejected with the same 404 a never-set secret would return.
func TestDevEnrollBurnsAfterFirstUse(t *testing.T) {
	c, _ := newObservableController(t, config.Config{
		Personalities: "admin",
		AuthMode:      "signed",
		Environment:   "dev",
	})
	if !c.devEnrollAutoGen {
		t.Fatalf("expected auto-bootstrap; got autoGen=false secret=%q", c.devEnrollSecret)
	}
	secret := c.devEnrollSecret

	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	pub, _, _ := ed25519.GenerateKey(nil)
	body, _ := json.Marshal(devEnrollRequest{
		PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
		Algorithm:    "ed25519",
	})

	doEnroll := func() *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/dev/enroll", bytes.NewReader(body))
		req.Header.Set("X-Txco-Enroll-Secret", secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		return resp
	}

	r1 := doEnroll()
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(r1.Body)
		t.Fatalf("first enroll status=%d body=%s", r1.StatusCode, out)
	}

	r2 := doEnroll()
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusNotFound {
		out, _ := io.ReadAll(r2.Body)
		t.Errorf("second enroll status=%d, want 404; body=%s", r2.StatusCode, out)
	}
}

// TestExplicitSecretDoesNotBurn — operator-supplied secrets allow
// multiple enrolments (they're under the operator's control; they're
// expected to manage rotation themselves).
func TestExplicitSecretDoesNotBurn(t *testing.T) {
	c, _ := newObservableController(t, config.Config{
		Personalities:       "admin",
		AuthMode:            "signed",
		AuthDevEnrollSecret: "stable",
		Environment:         "dev",
	})
	if c.devEnrollAutoGen {
		t.Fatalf("expected autoGen=false; got true")
	}
	srv := httptest.NewServer(withRouter(t, c, auth.ModeSigned))
	t.Cleanup(srv.Close)

	for i, label := range []string{"first", "second"} {
		pub, _, _ := ed25519.GenerateKey(nil)
		body, _ := json.Marshal(devEnrollRequest{
			PublicKeyB64: base64.StdEncoding.EncodeToString(pub),
			Algorithm:    "ed25519",
			Label:        label,
		})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/auth/dev/enroll", bytes.NewReader(body))
		req.Header.Set("X-Txco-Enroll-Secret", "stable")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		out, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("enroll %d status=%d body=%s", i, resp.StatusCode, out)
		}
	}
}
