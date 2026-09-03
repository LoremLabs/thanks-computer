package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

const rawTestMsg = "From: Owner <owner@example.com>\r\nTo: paris@pony.example.com\r\nSubject: verbatim\r\nMessage-ID: <v1@example.com>\r\nDate: Thu, 03 Sep 2026 10:00:00 +0000\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"b1\"\r\n\r\n--b1\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nexact bytes\r\n--b1\r\nContent-Type: application/pdf; name=\"guide.pdf\"\r\nContent-Disposition: attachment; filename=\"guide.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n--b1--\r\n"

func provision(t *testing.T, d imapDeps) {
	t.Helper()
	out := callIMAP(t, imapAccount, d, "acme", `{"username":"paris@pony.example.com"}`)
	if gjson.Get(out, "_imap.error").Exists() {
		t.Fatalf("provision: %s", out)
	}
}

func TestIMAPMailboxOp(t *testing.T) {
	d := newIMAPDeps(t, map[string]string{"pony.example.com": "acme"})
	provision(t, d)
	ctx := context.Background()

	out := callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","name":"Brain/Knowledge","role":"knowledge","attrs":["\\Archive"],"policy":{"append":"stack"}}`)
	if gjson.Get(out, "_imap.error").Exists() || !gjson.Get(out, "_imap.created").Bool() || gjson.Get(out, "_imap.role").String() != "knowledge" ||
		gjson.Get(out, "_imap.policy.append").String() != "stack" || gjson.Get(out, "_imap.attrs.0").String() != `\Archive` {
		t.Fatalf("create = %s", out)
	}
	id := gjson.Get(out, "_imap.id").String()
	// Idempotent by name: no create, role kept; policy updated by id.
	out = callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","id":"`+id+`","policy":{"append":"observe","flags":"observe"}}`)
	if gjson.Get(out, "_imap.created").Bool() || gjson.Get(out, "_imap.policy.flags").String() != "observe" || gjson.Get(out, "_imap.role").String() != "knowledge" {
		t.Errorf("update = %s", out)
	}
	out = callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","name":"Brain/Knowledge","policy":{"append":"maybe"}}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_invalid_arg" {
		t.Errorf("bad policy = %s", out)
	}
	// Rename keeps id + role; the subtree follows.
	callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","name":"Brain/Knowledge/Old"}`)
	out = callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","name":"Brain/Knowledge","rename_to":"Docs"}`)
	if gjson.Get(out, "_imap.id").String() != id || gjson.Get(out, "_imap.name").String() != "Docs" {
		t.Errorf("rename = %s", out)
	}
	if _, ok, _ := d.store.GetMailbox(ctx, "acme", "paris@pony.example.com", "Docs/Old"); !ok {
		t.Error("subtree did not follow the rename")
	}
	// role: lookup by role now finds Docs.
	out = callIMAP(t, imapMessages, d, "acme", `{"username":"paris@pony.example.com","mailbox":"role:knowledge"}`)
	if gjson.Get(out, "_imap.mailbox").String() != "Docs" {
		t.Errorf("by role = %s", out)
	}
	// Reset bumps uidvalidity; delete soft-deletes; INBOX refuses both.
	before := gjson.Get(out, "_imap.uidvalidity").Uint()
	out = callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","name":"Docs","reset":true}`)
	if gjson.Get(out, "_imap.uidvalidity").Uint() <= before && before != 0 {
		t.Errorf("reset = %s", out)
	}
	out = callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","name":"Docs","delete":true}`)
	if !gjson.Get(out, "_imap.deleted").Bool() {
		t.Errorf("delete = %s", out)
	}
	out = callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","name":"INBOX","delete":true}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_invalid_arg" {
		t.Errorf("delete INBOX = %s", out)
	}
	out = callIMAP(t, imapMailbox, d, "acme", `{"username":"ghost@pony.example.com","name":"X"}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_no_account" {
		t.Errorf("no account = %s", out)
	}
	// list: INBOX + Docs/Old (Docs itself deleted), counts present.
	out = callIMAP(t, imapList, d, "acme", `{"username":"paris@pony.example.com"}`)
	if gjson.Get(out, "_imap.count").Int() != 2 || gjson.Get(out, "_imap.mailboxes.1.name").String() != "INBOX" || !gjson.Get(out, "_imap.mailboxes.1.messages").Exists() {
		t.Errorf("list = %s", out)
	}
	out = callIMAP(t, imapList, d, "acme", `{"username":"paris@pony.example.com","prefix":"Docs/"}`)
	if gjson.Get(out, "_imap.count").Int() != 1 {
		t.Errorf("list prefix = %s", out)
	}
}

func TestIMAPMessageOps(t *testing.T) {
	d := newIMAPDeps(t, map[string]string{"pony.example.com": "acme"})
	provision(t, d)

	for i, subj := range []string{"one", "two", "three"} {
		out := callIMAP(t, imapAppend, d, "acme", `{"username":"paris@pony.example.com","object_key":"k`+string(rune('0'+i))+`","message":{"subject":"`+subj+`","text":"body `+subj+`"},"flags":["$Todo"]}`)
		if gjson.Get(out, "_imap.error").Exists() {
			t.Fatal(out)
		}
	}
	// messages: windowed.
	out := callIMAP(t, imapMessages, d, "acme", `{"username":"paris@pony.example.com","limit":2}`)
	if gjson.Get(out, "_imap.count").Int() != 2 || gjson.Get(out, "_imap.next").Int() != 2 || gjson.Get(out, "_imap.items.0.subject").String() != "one" ||
		gjson.Get(out, "_imap.items.0.flags.0").String() != "$Todo" {
		t.Errorf("messages = %s", out)
	}
	out = callIMAP(t, imapMessages, d, "acme", `{"username":"paris@pony.example.com","after":2}`)
	if gjson.Get(out, "_imap.count").Int() != 1 || gjson.Get(out, "_imap.items.0.uid").Int() != 3 || gjson.Get(out, "_imap.next").Int() != 0 {
		t.Errorf("messages after = %s", out)
	}
	// flags: add/remove by object_key.
	out = callIMAP(t, imapFlags, d, "acme", `{"username":"paris@pony.example.com","object_key":"k1","add":["$Done","\\Seen"],"remove":["$Todo"]}`)
	if gjson.Get(out, "_imap.uid").Int() != 2 || strings.Join(gjsonStrings(out, "_imap.flags"), ",") != `$Done,\Seen` {
		t.Errorf("flags = %s", out)
	}
	out = callIMAP(t, imapMessages, d, "acme", `{"username":"paris@pony.example.com","flags":["$Todo"]}`)
	if gjson.Get(out, "_imap.count").Int() != 2 {
		t.Errorf("flag filter = %s", out)
	}
	// get: record headers/text; raw renders.
	out = callIMAP(t, imapGet, d, "acme", `{"username":"paris@pony.example.com","uid":1,"raw":true}`)
	if gjson.Get(out, "_imap.headers.Subject").String() != "one" || gjson.Get(out, "_imap.text").String() != "body one" || gjson.Get(out, "_imap.kind").String() != "record" {
		t.Errorf("get = %s", out)
	}
	raw, _ := base64.StdEncoding.DecodeString(gjson.Get(out, "_imap.raw").String())
	if !strings.Contains(string(raw), "Subject: one\r\n") {
		t.Errorf("raw = %q", raw)
	}
	// remove: by uid, then missing → removed:false.
	out = callIMAP(t, imapRemove, d, "acme", `{"username":"paris@pony.example.com","uid":1}`)
	if !gjson.Get(out, "_imap.removed").Bool() {
		t.Errorf("remove = %s", out)
	}
	out = callIMAP(t, imapRemove, d, "acme", `{"username":"paris@pony.example.com","uid":1}`)
	if gjson.Get(out, "_imap.removed").Bool() || gjson.Get(out, "_imap.error").Exists() {
		t.Errorf("remove again = %s", out)
	}
	out = callIMAP(t, imapGet, d, "acme", `{"username":"paris@pony.example.com","uid":1}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_no_message" {
		t.Errorf("get removed = %s", out)
	}
}

func TestIMAPAppendVerbatimAndBlobFromSha(t *testing.T) {
	d := newIMAPDeps(t, map[string]string{"pony.example.com": "acme"})
	provision(t, d)
	ctx := context.Background()

	in := `{"mail":{"raw":"` + base64.StdEncoding.EncodeToString([]byte(rawTestMsg)) + `"}}`
	out := callIMAPIn(t, imapAppend, d, "acme", `{"username":"paris@pony.example.com","object_key":"v1","from":".mail.raw","flags":["\\Seen"]}`, in)
	if gjson.Get(out, "_imap.error").Exists() || gjson.Get(out, "_imap.kind").String() != "verbatim" || gjson.Get(out, "_imap.uid").Int() != 1 {
		t.Fatalf("verbatim append = %s", out)
	}
	sha := gjson.Get(out, "_imap.sha256").String()
	// The exact bytes come back; parts are owned + addressable.
	out = callIMAP(t, imapGet, d, "acme", `{"username":"paris@pony.example.com","object_key":"v1","raw":true}`)
	raw, _ := base64.StdEncoding.DecodeString(gjson.Get(out, "_imap.raw").String())
	if string(raw) != rawTestMsg || gjson.Get(out, "_imap.text").String() != "exact bytes" || gjson.Get(out, "_imap.headers.subject").String() != "verbatim" {
		t.Errorf("get verbatim = %s", out)
	}
	partSha := gjson.Get(out, "_imap.parts.0.sha256").String()
	if partSha == "" {
		t.Fatalf("no part sha: %s", out)
	}
	if _, owned, _ := d.ix.GetSha(ctx, "acme", partSha); !owned {
		t.Error("part not owned by the tenant")
	}
	// blob/put from_sha names the part under the tenant's scheme, no bytes.
	bd := blobDeps{fcas: d.fcas, ix: d.ix, maxBytes: 1 << 20, now: d.now}
	bout := callBlob(t, blobPut, bd, "acme", `{"name":"docs/guide.pdf","from_sha":"`+partSha+`","grants":[]}`, "")
	if gjson.Get(bout, "blob.error").Exists() || gjson.Get(bout, "_blob.sha256").String() != partSha || !gjson.Get(bout, "_blob.existed").Bool() {
		t.Errorf("blob from_sha = %s", bout)
	}
	bout = callBlob(t, blobPut, bd, "rival", `{"name":"docs/guide.pdf","from_sha":"`+partSha+`"}`, "")
	if gjson.Get(bout, "blob.error.code").String() != "txco_blob_not_found" {
		t.Errorf("cross-tenant from_sha = %s", bout)
	}
	bout = callBlob(t, blobPut, bd, "acme", `{"from_sha":"`+partSha+`"}`, "")
	if gjson.Get(bout, "blob.error.code").String() != "txco_blob_invalid_arg" {
		t.Errorf("from_sha without name = %s", bout)
	}
	// imap/append from_sha: an owned RFC 5322 object, into a second mailbox.
	callIMAP(t, imapMailbox, d, "acme", `{"username":"paris@pony.example.com","name":"Archive"}`)
	out = callIMAP(t, imapAppend, d, "acme", `{"username":"paris@pony.example.com","mailbox":"Archive","object_key":"v1","from_sha":"`+sha+`"}`)
	if gjson.Get(out, "_imap.error").Exists() || gjson.Get(out, "_imap.mailbox").String() != "Archive" || gjson.Get(out, "_imap.sha256").String() != sha {
		t.Errorf("from_sha append = %s", out)
	}
	out = callIMAP(t, imapAppend, d, "rival", `{"username":"paris@pony.example.com","object_key":"x","from_sha":"`+sha+`"}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_no_account" {
		t.Errorf("rival from_sha = %s", out)
	}
	out = callIMAP(t, imapAppend, d, "acme", `{"username":"paris@pony.example.com","object_key":"x","from_sha":"`+strings.Repeat("0", 64)+`"}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_no_message" {
		t.Errorf("unowned from_sha = %s", out)
	}
	out = callIMAP(t, imapAppend, d, "acme", `{"username":"paris@pony.example.com","object_key":"x","from":".nope","message":{"text":"x"}}`)
	if gjson.Get(out, "_imap.error.code").String() != "txco_imap_invalid_arg" {
		t.Errorf("both forms = %s", out)
	}
	_ = chimap.KindVerbatim
}

func callIMAPIn(t *testing.T, fn imapHandler, d imapDeps, tenant, metaJSON, inJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processorWithTenant(ctx, tenant)
	}
	ctx = operationWithMeta(ctx, metaJSON)
	pl, err := fn(ctx, d, []byte(inJSON))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

var (
	processorWithTenant = processor.WithTenant
	operationWithMeta   = operation.WithMeta
)

func gjsonStrings(raw, path string) []string {
	var out []string
	for _, v := range gjson.Get(raw, path).Array() {
		out = append(out, v.String())
	}
	return out
}
