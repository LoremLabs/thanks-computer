package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// setPolicy calls PUT /secrets/{name}/policy as the tenant's admin, or with
// exactly the capabilities given.
func setPolicy(t *testing.T, c *Controller, name, query, body string, caps ...string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := muxVars(httptest.NewRequest(http.MethodPut,
		"/v1/tenants/default/secrets/"+name+"/policy"+query, bytes.NewBufferString(body)),
		map[string]string{"name": name})
	if caps != nil {
		r = withTenantCapsCtx(r, testTenantID, caps)
	} else {
		r = withTenantAdminCtx(r, testTenantID)
	}
	c.handleSetSecretPolicy(w, r)
	return w
}

func TestSetSecretPolicy(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	ctx := context.Background()
	if _, err := store.CreateSecret(ctx, testTenantID, nil, "DB_DSN", "the database", "actor_test", []byte("v1")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A secret starts closed, and says so.
	w := httptest.NewRecorder()
	c.handleShowSecret(w, withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/secrets/DB_DSN", nil), map[string]string{"name": "DB_DSN"}), testTenantID))
	var shown secretResponse
	_ = json.Unmarshal(w.Body.Bytes(), &shown)
	if w.Code != http.StatusOK || shown.Secret.Pull != "none" {
		t.Fatalf("show: %d pull=%q body=%s", w.Code, shown.Secret.Pull, w.Body.String())
	}

	w = setPolicy(t, c, "DB_DSN", "", `{"pull":"reviewed"}`)
	var resp secretResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Secret.Pull != "reviewed" {
		t.Fatalf("set: %d body=%s", w.Code, w.Body.String())
	}
	// Nothing else about the secret moved, the description least of all.
	if resp.Secret.Description != "the database" || resp.Secret.VersionNo != 1 {
		t.Errorf("setting the policy changed the secret: %+v", resp.Secret)
	}
	if strings.Contains(w.Body.String(), "v1") && strings.Contains(w.Body.String(), `"value"`) {
		t.Errorf("the response carries a value: %s", w.Body.String())
	}

	// The list carries it too.
	w = httptest.NewRecorder()
	c.handleListSecrets(w, withTenantAdminCtx(httptest.NewRequest(http.MethodGet,
		"/v1/tenants/default/secrets", nil), testTenantID))
	var list listSecretsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Secrets) != 1 || list.Secrets[0].Pull != "reviewed" {
		t.Errorf("list: %s", w.Body.String())
	}

	// A PATCH of the description leaves the policy alone.
	body, _ := json.Marshal(updateSecretDescriptionRequest{Description: "renamed"})
	w = httptest.NewRecorder()
	c.handleUpdateSecretDescription(w, withTenantAdminCtx(muxVars(httptest.NewRequest(http.MethodPatch,
		"/v1/tenants/default/secrets/DB_DSN", bytes.NewBuffer(body)), map[string]string{"name": "DB_DSN"}), testTenantID))
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusOK || resp.Secret.Pull != "reviewed" || resp.Secret.Description != "renamed" {
		t.Errorf("patch after a policy: %d body=%s", w.Code, w.Body.String())
	}

	for name, tc := range map[string]struct {
		body string
		code int
		want string
	}{
		"an unknown policy":    {`{"pull":"all"}`, http.StatusBadRequest, "invalid_pull"},
		"an empty policy":      {`{"pull":""}`, http.StatusBadRequest, "invalid_pull"},
		"no policy":            {`{}`, http.StatusBadRequest, "invalid_pull"},
		"a misspelled field":   {`{"pul":"any"}`, http.StatusBadRequest, "invalid_body"},
		"a description too":    {`{"pull":"any","description":"x"}`, http.StatusBadRequest, "invalid_body"},
		"not JSON":             {`pull=any`, http.StatusBadRequest, "invalid_body"},
		"an empty body":        {``, http.StatusBadRequest, "invalid_body"},
		"a policy as a bool":   {`{"pull":true}`, http.StatusBadRequest, "invalid_body"},
		"a policy in capitals": {`{"pull":"ANY"}`, http.StatusBadRequest, "invalid_pull"},
	} {
		w := setPolicy(t, c, "DB_DSN", "", tc.body)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: got %d %s, want %d %s", name, w.Code, w.Body.String(), tc.code, tc.want)
		}
	}
	if meta, _ := store.LookupSecretMetadata(ctx, testTenantID, nil, "DB_DSN"); meta.Pull != secrets.PullReviewed {
		t.Errorf("a refused request changed the policy to %q", meta.Pull)
	}

	if w := setPolicy(t, c, "MISSING", "", `{"pull":"any"}`); w.Code != http.StatusNotFound {
		t.Errorf("a missing secret: %d %s", w.Code, w.Body.String())
	}
	if w := setPolicy(t, c, "bad-name", "", `{"pull":"any"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a bad name: %d %s", w.Code, w.Body.String())
	}
}

func TestSetSecretPolicyIsPerScope(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	ctx := context.Background()
	web := "web"
	if _, err := store.CreateSecret(ctx, testTenantID, nil, "API_KEY", "", "a", []byte("wide")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSecret(ctx, testTenantID, &web, "API_KEY", "", "a", []byte("web")); err != nil {
		t.Fatal(err)
	}
	if w := setPolicy(t, c, "API_KEY", "?stack=web", `{"pull":"any"}`); w.Code != http.StatusOK {
		t.Fatalf("set on the stack's own: %d %s", w.Code, w.Body.String())
	}
	if meta, _ := store.LookupSecretMetadata(ctx, testTenantID, &web, "API_KEY"); meta.Pull != secrets.PullAny {
		t.Errorf("the stack's own secret: %q", meta.Pull)
	}
	if meta, _ := store.LookupSecretMetadata(ctx, testTenantID, nil, "API_KEY"); meta.Pull != secrets.PullNone {
		t.Errorf("the tenant-wide secret was opened too: %q", meta.Pull)
	}
	if w := setPolicy(t, c, "API_KEY", "?stack=ingest", `{"pull":"any"}`); w.Code != http.StatusNotFound {
		t.Errorf("a stack that has no such secret: %d %s", w.Code, w.Body.String())
	}
}

func TestSetSecretPolicyNeedsTheWriteCapability(t *testing.T) {
	c := newTestControllerWithSecrets(t)
	store := c.pu.Secrets.Store()
	if _, err := store.CreateSecret(context.Background(), testTenantID, nil, "API_KEY", "", "a", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if w := setPolicy(t, c, "API_KEY", "", `{"pull":"any"}`, "secret:*:read"); w.Code != http.StatusForbidden {
		t.Errorf("with read only: %d %s", w.Code, w.Body.String())
	}
	if meta, _ := store.LookupSecretMetadata(context.Background(), testTenantID, nil, "API_KEY"); meta.Pull != secrets.PullNone {
		t.Errorf("a forbidden request changed the policy to %q", meta.Pull)
	}
	if w := setPolicy(t, c, "API_KEY", "", `{"pull":"any"}`, "secret:*:write"); w.Code != http.StatusOK {
		t.Errorf("with write: %d %s", w.Code, w.Body.String())
	}

	off := newTestController(t, config.Config{Personalities: "admin"})
	if w := setPolicy(t, off, "API_KEY", "", `{"pull":"any"}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("with no secret store: %d %s", w.Code, w.Body.String())
	}
}
