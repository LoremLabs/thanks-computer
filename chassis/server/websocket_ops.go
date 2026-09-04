package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	websocketp "github.com/loremlabs/thanks-computer/chassis/server/personality/websocket"
)

// websocket_ops.go — the txco://websocket/* family, the stack's side of a
// WebSocket session the `websocket` personality owns:
//
//	txco://websocket/accept  in the upgrade request's own run: take this
//	                         connection (WITH state, origins, subprotocols,
//	                         events, idle_timeout, max_message_bytes)
//	txco://websocket/send    write a message to a session by id
//	txco://websocket/reply   send to the session that produced this envelope
//	txco://websocket/close   close a session (code, reason)
//
// Scoping is trusted: the tenant comes from processor.TenantScope(ctx) and
// the originating inlet from processor.SourceScope(ctx), never from a
// mutable `_txc.*` field. `accept` only records for a session id the web
// head stamped on an upgrade request; a session of another tenant is
// indistinguishable from a missing one.
//
// Output lands under `into` (default `_websocket`); errors as
// `<into>.error.{code,message}` with a nil Go error, so authors branch with
// `WHEN ._websocket.error.code != ""` and the run continues.

type websocketDeps struct {
	reg websocketp.Registry // nil ⇒ txco_websocket_disabled
}

func websocketInto(meta []byte) string {
	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_websocket"
	}
	return into
}

func websocketErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

func websocketPrelude(ctx context.Context, d websocketDeps) (tenant string, meta []byte, into string, errPayload event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = websocketInto(meta)
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", nil, into, websocketErr(into, "txco_websocket_no_tenant", "no tenant in request scope"), false
	}
	if d.reg == nil || !d.reg.Enabled() {
		return "", nil, into, websocketErr(into, "txco_websocket_disabled", "the websocket personality is not enabled on this node (add `websocket` to --personalities alongside `web`)"), false
	}
	return tenant, meta, into, event.Payload{}, true
}

// websocketRegistryErr maps a Registry error to the op error shape.
func websocketRegistryErr(into string, err error) event.Payload {
	switch {
	case errors.Is(err, websocketp.ErrDisabled):
		return websocketErr(into, "txco_websocket_disabled", err.Error())
	case errors.Is(err, websocketp.ErrSessionNotFound):
		return websocketErr(into, "txco_websocket_session_not_found", "no live session with that id for this tenant on this node")
	case errors.Is(err, websocketp.ErrSessionClosed):
		return websocketErr(into, "txco_websocket_session_closed", err.Error())
	case errors.Is(err, websocketp.ErrWriteTimeout):
		return websocketErr(into, "txco_websocket_write_timeout", "the client did not accept the message before the write timeout; the session was closed")
	case errors.Is(err, websocketp.ErrMessageTooLarge):
		return websocketErr(into, "txco_websocket_message_too_large", err.Error())
	case errors.Is(err, websocketp.ErrPendingFull):
		return websocketErr(into, "txco_websocket_bad_state", err.Error())
	}
	return websocketErr(into, "txco_websocket_error", err.Error())
}

// websocketAccept records the stack's decision to take the connection. It
// runs inside the upgrade request's own (http-sourced) run; the web head
// claims the decision when the run ends and performs the handshake.
func websocketAccept(ctx context.Context, d websocketDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := websocketPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	sid := gjson.GetBytes(in, "_txc.websocket.session.id").String()
	if processor.SourceScope(ctx) != "http" || !gjson.GetBytes(in, "_txc.websocket.upgrade").Bool() || sid == "" {
		return websocketErr(into, "txco_websocket_not_upgrade", "accept only applies inside the run of a WebSocket upgrade request (@websocket.upgrade == true)"), nil
	}
	stack := gjson.GetBytes(in, "_txc.stack").String()
	if stack == "" {
		stack = gjson.GetBytes(in, "_txc.route.stack").String()
	}
	if stack == "" {
		return websocketErr(into, "txco_websocket_bad_state", "no stack is pinned on this run"), nil
	}
	acc := websocketp.Accept{
		Tenant:           tenant,
		Stack:            stack,
		Ingress:          gjson.GetBytes(in, "_txc.ingress").String(),
		HostnameVerified: gjson.GetBytes(in, "_txc.hostname_verified").Bool(),
		Events:           map[string]bool{},
	}
	if acc.Ingress == "" {
		acc.Ingress = websocketp.Source
	}

	// WITH values resolve against the envelope: a missing path arrives as
	// JSON null, a failed builtin empties the whole clause — so every
	// option is optional, typed, and null-tolerant.
	if st := gjson.GetBytes(meta, "state"); st.Exists() && st.Type != gjson.Null {
		if !st.IsObject() {
			return websocketErr(into, "txco_websocket_bad_argument", "`state` must be an object"), nil
		}
		clean, err := websocketStripNulls(st.Raw)
		if err != nil {
			return websocketErr(into, "txco_websocket_bad_argument", "`state`: "+err.Error()), nil
		}
		if len(clean) > websocketp.StateMaxBytes {
			return websocketErr(into, "txco_websocket_state_too_large",
				fmt.Sprintf("`state` is %d bytes; the limit is %d", len(clean), websocketp.StateMaxBytes)), nil
		}
		acc.State = clean
	}
	origins, err := websocketMetaStrings(meta, "origins")
	if err != nil {
		return websocketErr(into, "txco_websocket_bad_argument", err.Error()), nil
	}
	for _, o := range origins {
		if o == "*" {
			acc.AnyOrigin = true
			continue
		}
		acc.Origins = append(acc.Origins, o)
	}
	if acc.Subprotocols, err = websocketMetaStrings(meta, "subprotocols"); err != nil {
		return websocketErr(into, "txco_websocket_bad_argument", err.Error()), nil
	}
	acc.SubprotocolRequired = gjson.GetBytes(meta, "subprotocol_required").Bool()
	if acc.SubprotocolRequired && len(acc.Subprotocols) == 0 {
		return websocketErr(into, "txco_websocket_bad_argument", "`subprotocol_required` needs a `subprotocols` list"), nil
	}
	events, err := websocketMetaStrings(meta, "events")
	if err != nil {
		return websocketErr(into, "txco_websocket_bad_argument", err.Error()), nil
	}
	for _, e := range events {
		if e != websocketp.EventClose {
			return websocketErr(into, "txco_websocket_bad_argument", fmt.Sprintf("unknown event %q (known: %s)", e, websocketp.EventClose)), nil
		}
		acc.Events[e] = true
	}
	if acc.IdleTimeout, err = websocketMetaDuration(meta, "idle_timeout"); err != nil {
		return websocketErr(into, "txco_websocket_bad_argument", err.Error()), nil
	}
	if v := gjson.GetBytes(meta, "max_message_bytes"); v.Exists() && v.Type != gjson.Null {
		if v.Type != gjson.Number || v.Int() <= 0 {
			return websocketErr(into, "txco_websocket_bad_argument", "`max_message_bytes` must be a positive number"), nil
		}
		acc.MaxMessageBytes = v.Int()
	}

	if err := d.reg.RecordAccept(sid, acc); err != nil {
		return websocketRegistryErr(into, err), nil
	}
	raw, _ := sjson.Set(`{}`, into+".accepted", true)
	raw, _ = sjson.Set(raw, into+".session.id", sid)
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// websocketSend writes to a session named by `session_id`.
func websocketSend(ctx context.Context, d websocketDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := websocketPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	sid := gjson.GetBytes(meta, "session_id").String()
	if sid == "" {
		return websocketErr(into, "txco_websocket_bad_argument", "missing `session_id`"), nil
	}
	return websocketDeliver(ctx, d, tenant, meta, into, sid)
}

// websocketReply writes to the session that produced this envelope. Only
// meaningful in a websocket-sourced run (or a resumed continuation of one).
func websocketReply(ctx context.Context, d websocketDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := websocketPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	sid := gjson.GetBytes(in, "_txc.websocket.session.id").String()
	if processor.SourceScope(ctx) != websocketp.Source || sid == "" {
		return websocketErr(into, "txco_websocket_not_session_run",
			"reply only applies inside a websocket session's run; use txco://websocket/send with an explicit session_id elsewhere"), nil
	}
	return websocketDeliver(ctx, d, tenant, meta, into, sid)
}

func websocketDeliver(ctx context.Context, d websocketDeps, tenant string, meta []byte, into, sid string) (event.Payload, error) {
	typ, data, ep, ok := websocketPayload(meta, into)
	if !ok {
		return ep, nil
	}
	if err := d.reg.Send(ctx, tenant, sid, typ, data); err != nil {
		return websocketRegistryErr(into, err), nil
	}
	raw, _ := sjson.Set(`{}`, into+".sent.session_id", sid)
	raw, _ = sjson.Set(raw, into+".sent.bytes", len(data))
	raw, _ = sjson.Set(raw, into+".sent.type", typ.String())
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// websocketClose starts the close handshake on a session.
func websocketClose(ctx context.Context, d websocketDeps, in []byte) (event.Payload, error) {
	tenant, meta, into, ep, ok := websocketPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	sid := gjson.GetBytes(meta, "session_id").String()
	if sid == "" && processor.SourceScope(ctx) == websocketp.Source {
		sid = gjson.GetBytes(in, "_txc.websocket.session.id").String()
	}
	if sid == "" {
		return websocketErr(into, "txco_websocket_bad_argument", "missing `session_id` (implicit only inside a websocket session's run)"), nil
	}
	code := 1000
	if v := gjson.GetBytes(meta, "code"); v.Exists() && v.Type != gjson.Null {
		if v.Type != gjson.Number {
			return websocketErr(into, "txco_websocket_invalid_close_code", "`code` must be a number"), nil
		}
		code = int(v.Int())
	}
	if !websocketp.ValidCloseCode(code) {
		return websocketErr(into, "txco_websocket_invalid_close_code",
			fmt.Sprintf("close code %d is not one a stack may send (1000, 1001, 1003, 1007-1011, 3000-4999)", code)), nil
	}
	reason := gjson.GetBytes(meta, "reason").String()
	if len(reason) > 123 {
		return websocketErr(into, "txco_websocket_bad_argument", "`reason` must be at most 123 bytes"), nil
	}
	if err := d.reg.CloseSession(ctx, tenant, sid, code, reason); err != nil {
		return websocketRegistryErr(into, err), nil
	}
	raw, _ := sjson.Set(`{}`, into+".closed.session_id", sid)
	raw, _ = sjson.Set(raw, into+".closed.code", code)
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// websocketPayload reads exactly one of `text` (a string as-is; any other
// JSON value serialized as text) or `data` (base64 → a binary frame).
func websocketPayload(meta []byte, into string) (websocketp.MessageType, []byte, event.Payload, bool) {
	text := gjson.GetBytes(meta, "text")
	data := gjson.GetBytes(meta, "data")
	hasText := text.Exists() && text.Type != gjson.Null
	hasData := data.Exists() && data.Type != gjson.Null
	switch {
	case hasText && hasData:
		return 0, nil, websocketErr(into, "txco_websocket_bad_argument", "give `text` or `data`, not both"), false
	case hasText:
		if text.Type == gjson.String {
			return websocketp.MessageText, []byte(text.String()), event.Payload{}, true
		}
		return websocketp.MessageText, []byte(text.Raw), event.Payload{}, true
	case hasData:
		if data.Type != gjson.String {
			return 0, nil, websocketErr(into, "txco_websocket_bad_argument", "`data` must be a base64 string"), false
		}
		b, err := base64.StdEncoding.DecodeString(data.String())
		if err != nil {
			return 0, nil, websocketErr(into, "txco_websocket_bad_argument", "`data` is not valid base64: "+err.Error()), false
		}
		return websocketp.MessageBinary, b, event.Payload{}, true
	}
	return 0, nil, websocketErr(into, "txco_websocket_bad_argument", "missing `text` (or `data`) — a WITH path that resolved to nothing arrives as null"), false
}

// websocketMetaStrings reads an optional list option: an array of non-empty
// strings, or one string. Null/absent ⇒ nil.
func websocketMetaStrings(meta []byte, key string) ([]string, error) {
	v := gjson.GetBytes(meta, key)
	if !v.Exists() || v.Type == gjson.Null {
		return nil, nil
	}
	var out []string
	add := func(r gjson.Result) error {
		if r.Type != gjson.String || strings.TrimSpace(r.String()) == "" {
			return fmt.Errorf("`%s` entries must be non-empty strings", key)
		}
		out = append(out, strings.TrimSpace(r.String()))
		return nil
	}
	if v.IsArray() {
		var err error
		v.ForEach(func(_, r gjson.Result) bool {
			err = add(r)
			return err == nil
		})
		return out, err
	}
	if err := add(v); err != nil {
		return nil, err
	}
	return out, nil
}

// websocketMetaDuration reads an optional duration: a number is
// milliseconds, a string is a Go duration (the WITH timeout convention).
func websocketMetaDuration(meta []byte, key string) (time.Duration, error) {
	v := gjson.GetBytes(meta, key)
	if !v.Exists() || v.Type == gjson.Null {
		return 0, nil
	}
	var d time.Duration
	switch v.Type {
	case gjson.Number:
		d = time.Duration(v.Int()) * time.Millisecond
	case gjson.String:
		p, err := time.ParseDuration(v.String())
		if err != nil {
			return 0, fmt.Errorf("`%s`: %v", key, err)
		}
		d = p
	default:
		return 0, fmt.Errorf("`%s` must be a duration string or milliseconds", key)
	}
	if d <= 0 {
		return 0, fmt.Errorf("`%s` must be positive", key)
	}
	return d, nil
}

// websocketStripNulls drops top-level null members (a WITH path that
// resolved to nothing) and re-serializes compactly.
func websocketStripNulls(raw string) (string, error) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", err
	}
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
