package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

func TestValidateStackFilePathCaps(t *testing.T) {
	for _, p := range []string{"CAPS/ai.chat.yaml", "CAPS/local.web.fetch.yaml", "CAPS/finish.yaml"} {
		if err := validateStackFilePath(p); err != nil {
			t.Errorf("validateStackFilePath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{
		"CAPS/ai.chat.json",   // wrong extension
		"CAPS/a/ai.chat.yaml", // no nesting
		"CAPS/Ai.chat.yaml",   // name rule
		"CAPS/.yaml",          // empty name
		"caps/ai.chat.yaml",   // exact-case channel
	} {
		if err := validateStackFilePath(p); err == nil {
			t.Errorf("validateStackFilePath(%q) = nil, want error", p)
		}
	}
}

// capsFixture: tenant tnt_default has stack `loop` (draft version 1) and an
// ACTIVE stack `other` (version 10); tenant tnt_acme has an active stack too.
func capsFixture(t *testing.T, c *Controller) {
	t.Helper()
	for _, s := range []string{
		`INSERT INTO stacks (stack_id, tenant_id, name, created_at) VALUES ('stk_loop','tnt_default','loop','t')`,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash) VALUES (1,'stk_loop',1,'draft','test','t','')`,
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES ('stk_other','tnt_default','other',10,'t')`,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash) VALUES (10,'stk_other',1,'draft','test','t','')`,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash) VALUES (11,'stk_other',2,'draft','test','t','')`,
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES ('stk_acme','tnt_acme','loop',20,'t')`,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at, manifest_hash) VALUES (20,'stk_acme',1,'draft','test','t','')`,
	} {
		if _, err := c.pu.RuntimeDB.ExecContext(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
}

func putCapFile(t *testing.T, c *Controller, version int, path, body, hash string) {
	t.Helper()
	if _, err := c.pu.RuntimeDB.ExecContext(context.Background(),
		`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (?, ?, ?, ?)`, version, path, body, hash); err != nil {
		t.Fatal(err)
	}
}

func TestDeepValidateCaps(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	capsFixture(t, c)
	const fleetHash = "f1ee7"
	c.fcas = &mapStore{m: map[string][]byte{fleetHash: []byte("entry: 7000\n")}}

	// What the tenant's other active stack declares; a superseded draft of
	// it and another tenant's stack declare names that must NOT collide.
	putCapFile(t, c, 10, "CAPS/mail.send.yaml", "entry: 100\n", "")
	putCapFile(t, c, 11, "CAPS/card.note.yaml", "entry: 100\n", "")
	putCapFile(t, c, 20, "CAPS/ai.chat.yaml", "entry: 100\n", "")

	for p, body := range map[string]string{
		"CAPS/ai.chat.yaml":   "description: Ask the model.\nentry: 7000\ninput:\n  messages:\n    required: true\n",
		"CAPS/card.note.yaml": "entry: 7000\n",
		"CAPS/unknown.yaml":   "entry: 7000\noutput: {}\n",
		"CAPS/noentry.yaml":   "description: nothing answers\n",
		"CAPS/noscope.yaml":   "entry: 7100\n",
		"CAPS/mail.send.yaml": "entry: 7000\n",
		"100/plain.txcl":      `EXEC "txco://copy" WITH from = "a", to = "b"`,
		"7000/run.txcl":       `EXEC "txco://copy" WITH from = "a", to = "b"`,
	} {
		putCapFile(t, c, 1, p, body, "")
	}
	// A fleet row: fingerprint only, the bytes in the CAS.
	putCapFile(t, c, 1, "CAPS/run.finish.yaml", "", fleetHash)

	issues := c.deepValidateCaps(ctx, "tnt_default", "stk_loop", 1)
	want := map[string]string{
		"CAPS/unknown.yaml":   "field output not found",
		"CAPS/noentry.yaml":   "entry is required",
		"CAPS/noscope.yaml":   "entry 7100 names no scope of this stack",
		"CAPS/mail.send.yaml": `already declared by the active stack "other"`,
	}
	if len(issues) != len(want) {
		t.Fatalf("want %d issues, got %+v", len(want), issues)
	}
	for _, i := range issues {
		sub, ok := want[i.Path]
		if !ok || !strings.Contains(i.Err, sub) {
			t.Errorf("unexpected issue %+v", i)
		}
	}

	// A fleet row whose bytes the CAS lacks is an issue, not a pass.
	putCapFile(t, c, 1, "CAPS/gone.yaml", "", "n0b0dy")
	found := false
	for _, i := range c.deepValidateCaps(ctx, "tnt_default", "stk_loop", 1) {
		if i.Path == "CAPS/gone.yaml" && strings.Contains(i.Err, "resolve declaration from the CAS") {
			found = true
		}
	}
	if !found {
		t.Fatal("a declaration missing from the CAS must be an issue")
	}

	// The stack's own active version declaring the same name is not a
	// collision: re-activating `other` with mail.send is fine.
	putCapFile(t, c, 11, "CAPS/mail.send.yaml", "entry: 100\n", "")
	putCapFile(t, c, 11, "100/plain.txcl", `EXEC "txco://copy" WITH from = "a", to = "b"`, "")
	if issues := c.deepValidateCaps(ctx, "tnt_default", "stk_other", 11); len(issues) != 0 {
		t.Fatalf("own active version must not collide: %+v", issues)
	}

	// A version without capabilities is a cheap no-op.
	if issues := c.deepValidateCaps(ctx, "tnt_default", "stk_other", 999); len(issues) != 0 {
		t.Fatalf("empty version: %+v", issues)
	}
}

func TestHandleListCaps(t *testing.T) {
	c := newTestController(t, config.Config{})
	capsFixture(t, c)
	const fleetHash = "f1ee7"
	c.fcas = &mapStore{m: map[string][]byte{fleetHash: []byte("description: Finish the run.\nentry: 7000\n")}}

	putCapFile(t, c, 10, "CAPS/mail.send.yaml", "description: Send a message.\nentry: 100\ninput:\n  to:\n    required: true\ntimeout: 60000\n", "")
	putCapFile(t, c, 10, "CAPS/run.finish.yaml", "", fleetHash)
	putCapFile(t, c, 10, "CAPS/broken.yaml", "entry: 100\noutput: {}\n", "")
	putCapFile(t, c, 10, "SANDBOXES/workstation.yaml", "capabilities: [mail.send]\n", "")
	putCapFile(t, c, 1, "CAPS/draft.only.yaml", "entry: 100\n", "") // not active
	putCapFile(t, c, 20, "CAPS/ai.chat.yaml", "entry: 100\n", "")   // another tenant

	get := func(r *http.Request) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c.handleListCaps(w, r)
		return w
	}
	w := get(withTenantCapsCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/caps", nil), "tnt_default", []string{"opstack:*:read"}))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp listCapsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 3 || len(resp.Caps) != 3 {
		t.Fatalf("want 3 caps, got %s", w.Body.String())
	}
	broken, mail, finish := resp.Caps[0], resp.Caps[1], resp.Caps[2]
	if broken.Name != "broken" || broken.Stack != "other" || !strings.Contains(broken.Err, "field output not found") {
		t.Errorf("broken row: %+v", broken)
	}
	if mail.Name != "mail.send" || mail.Stack != "other" || mail.Entry != 100 || mail.Stage != "other/100" ||
		mail.Description != "Send a message." || !mail.Input["to"].Required || mail.Timeout != 60000 || mail.Err != "" {
		t.Errorf("mail.send row: %+v", mail)
	}
	if finish.Name != "run.finish" || finish.Stage != "other/7000" || finish.Description != "Finish the run." {
		t.Errorf("run.finish (CAS) row: %+v", finish)
	}

	// No read capability: refused.
	w = get(withTenantCapsCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/caps", nil), "tnt_default", []string{"kv:*:read"}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("without opstack read: status %d", w.Code)
	}

	// A tenant with none answers an empty list, not null.
	w = get(withTenantCapsCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/x/caps", nil), "tnt_none", []string{"opstack:*:read"}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"caps":[]`) {
		t.Fatalf("empty tenant: %d %s", w.Code, w.Body.String())
	}
}
