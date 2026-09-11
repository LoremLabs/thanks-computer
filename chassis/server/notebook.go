package server

// txco://notebook/{append,read,export,list,delete} — the append-only record
// a stack writes and reads (chassis/notebook): per (tenant, namespace, name)
// a head row that allocates sequence numbers, and entries
// {seq, at, type, data, object_key} in that order. Task is state; a
// notebook is history.
//
//	append  WITH notebook, type, data?, object_key?, ttl?, namespace?
//	        → {seq, at, existed}. A duplicate object_key returns the
//	        ORIGINAL row with existed=true and writes nothing, so a retry
//	        is observationally identical to the first success.
//	read    WITH notebook, after?, since?, until?, tail?, type?, limit?
//	        → {entries[], next, cursor, count, truncated}, ALWAYS ascending
//	        by seq. `next` is non-empty only when the page was full (the
//	        `LOOP … UNTIL ._notebook.next == ""` drain); `cursor` is the
//	        position after the last entry (poll with after = cursor).
//	export  the same selection, format="ndjson" → the body on
//	        @web.res.body (content-type application/x-ndjson) + @halt, and
//	        {count, next, cursor, truncated, bytes} at `into`.
//	list    WITH prefix?, after?, limit? → {notebooks[], next, count}.
//	delete  WITH notebook → {deleted}.
//
// Cursors are opaque strings carrying the notebook's generation; a cursor
// from a notebook that was deleted and recreated answers
// txco_notebook_stale_cursor rather than silently re-reading new entries
// as old. `at` is chassis-assigned; a caller's own timestamps belong inside
// `data`.
//
// Scoping is trusted: tenant from processor.TenantScope(ctx), never a
// mutable _txc.* field; namespace from WITH `namespace`, else the routed
// stack's app namespace (the KV rule, appStackNamespace), else "default".
//
// Output lands under `into` (default `_notebook`); errors as
// `<into>.error.{code,message}` with a nil Go error, so authors branch with
// `WHEN ._notebook.error.code != ""` and the run continues. No record means
// no success response — a record that silently fails is worse than none.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	chnotebook "github.com/loremlabs/thanks-computer/chassis/notebook"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

type notebookDeps struct {
	store          *chnotebook.Store // nil ⇒ every op answers txco_notebook_disabled
	maxExportBytes int64             // cap for one export body; 0 = unlimited
}

func notebookInto(meta []byte) string {
	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_notebook"
	}
	return into
}

func notebookErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// notebookErrFrom maps a store error to the envelope by its code.
func notebookErrFrom(into string, err error) event.Payload {
	code := chnotebook.ErrorCode(err)
	if code == "" {
		code = "txco_notebook_store"
	}
	return notebookErr(into, code, err.Error())
}

// notebookPrelude is the common head of every handler: tenant, store,
// meta, into, namespace.
func notebookPrelude(ctx context.Context, d notebookDeps, in []byte) (tenant, ns string, meta []byte, into string, errPayload event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = notebookInto(meta)
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", "", nil, into, notebookErr(into, "txco_notebook_no_tenant", "no tenant in request scope"), false
	}
	if d.store == nil {
		return "", "", nil, into, notebookErr(into, "txco_notebook_disabled", "no notebook store on this node (it failed to open at boot; see --notebook-store)"), false
	}
	// Namespace: WITH `namespace`, else the routed stack — _txc.stack on the
	// mail/LMTP path, _txc.route.stack on the HTTP path (the same dual read
	// as kv.go) — collapsed to the app namespace so `<stack>/_mail` shares
	// the app's notebooks; chassis-reserved namespaces refused.
	ns = gjson.GetBytes(meta, "namespace").String()
	if ns == "" {
		ns = gjson.GetBytes(in, "_txc.stack").String()
	}
	if ns == "" {
		ns = gjson.GetBytes(in, "_txc.route.stack").String()
	}
	if ns == "" {
		ns = "default"
	}
	ns = appStackNamespace(ns)
	if kvstore.IsReservedNamespace(ns) {
		return "", "", nil, into, notebookErr(into, "txco_notebook_invalid_arg", fmt.Sprintf("namespace %q is reserved for the chassis", ns)), false
	}
	return tenant, ns, meta, into, event.Payload{}, true
}

// notebookStage labels fuel charges with the op identity the processor stamped.
func notebookStage(in []byte) string { return gjson.GetBytes(in, "_txc.op").String() }

// notebookChargeBytes meters bytes returned to the envelope (read) or the
// response (export) per MiB, rounded up. Appends pay only the dispatch:
// the per-entry cap bounds bytes in.
func notebookChargeBytes(ctx context.Context, n int64, in []byte) {
	mib := (n + (1 << 20) - 1) >> 20
	if mib > 0 {
		_ = processor.AddFuel(ctx, mib*processor.FuelCostNotebookPerMiB, notebookStage(in))
	}
}

func notebookRef(tenant, ns string, meta []byte) chnotebook.Ref {
	return chnotebook.Ref{Tenant: tenant, Namespace: ns, Name: gjson.GetBytes(meta, "notebook").String()}
}

// notebookReadReq lifts the read selection out of WITH. `after` is an
// opaque cursor (never a number); `since`/`until` accept any RFC 3339
// precision; `tail`, `limit` are counts.
func notebookReadReq(meta []byte, ref chnotebook.Ref) (chnotebook.ReadReq, error) {
	req := chnotebook.ReadReq{Ref: ref}
	if a := gjson.GetBytes(meta, "after").String(); a != "" {
		c, err := chnotebook.ParseCursor(a)
		if err != nil {
			return req, err
		}
		req.After = c
	}
	if v := gjson.GetBytes(meta, "since").String(); v != "" {
		t, err := chnotebook.ParseAt(v)
		if err != nil {
			return req, &chnotebook.InvalidArgError{Reason: "since must be an RFC 3339 timestamp"}
		}
		req.Since = t
	}
	if v := gjson.GetBytes(meta, "until").String(); v != "" {
		t, err := chnotebook.ParseAt(v)
		if err != nil {
			return req, &chnotebook.InvalidArgError{Reason: "until must be an RFC 3339 timestamp"}
		}
		req.Until = t
	}
	req.Tail = int(gjson.GetBytes(meta, "tail").Int())
	req.Type = gjson.GetBytes(meta, "type").String()
	req.Limit = int(gjson.GetBytes(meta, "limit").Int())
	return req, nil
}

// notebookEntryJSON renders one entry — the same object for a read's
// `entries[]` and an export's NDJSON line.
func notebookEntryJSON(e chnotebook.Entry) string {
	b := jsonx.NewObject()
	b.Set("seq", e.Seq)
	b.Set("at", chnotebook.FormatAt(e.At))
	b.Set("type", e.Type)
	b.SetRaw("data", string(e.Data))
	if e.ObjectKey != "" {
		b.Set("object_key", e.ObjectKey)
	}
	return b.String()
}

func notebookEntriesJSON(entries []chnotebook.Entry) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, e := range entries {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(notebookEntryJSON(e))
	}
	sb.WriteByte(']')
	return sb.String()
}

// notebookAppend records one entry. Result at `into`: {seq, at, existed}.
func notebookAppend(ctx context.Context, d notebookDeps, in []byte) (event.Payload, error) {
	tenant, ns, meta, into, ep, ok := notebookPrelude(ctx, d, in)
	if !ok {
		return ep, nil
	}
	req := chnotebook.AppendReq{
		Ref:       notebookRef(tenant, ns, meta),
		Type:      gjson.GetBytes(meta, "type").String(),
		ObjectKey: gjson.GetBytes(meta, "object_key").String(),
	}
	if dv := gjson.GetBytes(meta, "data"); dv.Exists() && dv.Type != gjson.Null {
		req.Data = json.RawMessage(dv.Raw) // objects, arrays and scalars pass through verbatim; a missing path (null) is "no data"
	}
	if ttl := gjson.GetBytes(meta, "ttl"); ttl.Exists() {
		secs := ttl.Int()
		if secs < 0 {
			return notebookErr(into, "txco_notebook_invalid_arg", "ttl must be a non-negative number of seconds"), nil
		}
		req.TTL = time.Duration(secs) * time.Second
	}
	r, err := d.store.Append(ctx, req)
	if err != nil {
		return notebookErrFrom(into, err), nil
	}
	resp := jsonx.NewObject()
	resp.Set(into+".seq", r.Seq)
	resp.Set(into+".at", chnotebook.FormatAt(r.At))
	resp.Set(into+".existed", r.Existed)
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// notebookRead returns one page as data. Result at `into`:
// {entries, next, cursor, count, truncated}.
func notebookRead(ctx context.Context, d notebookDeps, in []byte) (event.Payload, error) {
	tenant, ns, meta, into, ep, ok := notebookPrelude(ctx, d, in)
	if !ok {
		return ep, nil
	}
	req, err := notebookReadReq(meta, notebookRef(tenant, ns, meta))
	if err != nil {
		return notebookErrFrom(into, err), nil
	}
	r, err := d.store.Read(ctx, req)
	if err != nil {
		return notebookErrFrom(into, err), nil
	}
	entries := notebookEntriesJSON(r.Entries)
	notebookChargeBytes(ctx, int64(len(entries)), in)
	resp := jsonx.NewObject()
	resp.SetRaw(into+".entries", entries)
	resp.Set(into+".next", r.Next.Encode())
	resp.Set(into+".cursor", r.Cursor.Encode())
	resp.Set(into+".count", r.Count)
	resp.Set(into+".truncated", r.Truncated)
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

var errNotebookExportStop = errors.New("notebook export: window full")

// notebookExport writes the selection as NDJSON onto the HTTP response
// (@web.res.body base64, content-type application/x-ndjson) and halts —
// the web-render shape, so the body is served with a Content-Length
// rather than streamed. Bounded by `limit` rows and --notebook-max-export-bytes;
// when either cuts the export short, `truncated` is true, `next` (and the
// x-notebook-next header) is the cursor to resume from with `after`.
func notebookExport(ctx context.Context, d notebookDeps, in []byte) (event.Payload, error) {
	tenant, ns, meta, into, ep, ok := notebookPrelude(ctx, d, in)
	if !ok {
		return ep, nil
	}
	if f := gjson.GetBytes(meta, "format").String(); f != "" && f != "ndjson" {
		return notebookErr(into, "txco_notebook_invalid_arg", fmt.Sprintf("format %q is not supported (ndjson)", f)), nil
	}
	ref := notebookRef(tenant, ns, meta)
	req, err := notebookReadReq(meta, ref)
	if err != nil {
		return notebookErrFrom(into, err), nil
	}
	rowLimit := req.Limit
	req.Limit = 0 // ForEach pages at the node ceiling; the row cap is applied below
	head, exists, err := d.store.Stat(ctx, ref)
	if err != nil {
		return notebookErrFrom(into, err), nil
	}
	var buf bytes.Buffer
	var count int
	var last, next chnotebook.Cursor
	truncated := false
	if exists {
		err = d.store.ForEach(ctx, req, func(e chnotebook.Entry) error {
			line := notebookEntryJSON(e)
			if (rowLimit > 0 && count >= rowLimit) ||
				(d.maxExportBytes > 0 && int64(buf.Len()+len(line)+1) > d.maxExportBytes) {
				truncated = true
				return errNotebookExportStop
			}
			buf.WriteString(line)
			buf.WriteByte('\n')
			count++
			last = chnotebook.Cursor{Generation: head.Generation, Seq: e.Seq}
			return nil
		})
		if err != nil && !errors.Is(err, errNotebookExportStop) {
			return notebookErrFrom(into, err), nil
		}
	}
	if truncated {
		next = last
	}
	if buf.Len() == 0 {
		// An empty @web.res.body means "no body" to the web personality,
		// which would serve the envelope instead; an NDJSON reader skips a
		// blank line.
		buf.WriteByte('\n')
	}
	body := buf.Bytes()
	notebookChargeBytes(ctx, int64(len(body)), in)

	resp := `{}`
	resp, _ = sjson.Set(resp, "_txc.web.res.status", 200)
	resp, _ = sjson.Set(resp, "_txc.web.res.headers.content-type.0", "application/x-ndjson")
	if truncated {
		resp, _ = sjson.Set(resp, "_txc.web.res.headers.x-notebook-next.0", next.Encode())
	}
	resp, _ = sjson.Set(resp, "_txc.web.res.body", base64.StdEncoding.EncodeToString(body))
	resp, _ = sjson.Set(resp, "_txc.halt", true)
	resp, _ = sjson.Set(resp, into+".count", count)
	resp, _ = sjson.Set(resp, into+".next", next.Encode())
	resp, _ = sjson.Set(resp, into+".cursor", last.Encode())
	resp, _ = sjson.Set(resp, into+".truncated", truncated)
	resp, _ = sjson.Set(resp, into+".bytes", len(body))
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// notebookList enumerates the namespace's notebooks by name prefix. Result
// at `into`: {notebooks[{name, high_seq, ttl_secs, created_at, updated_at}], next, count}.
func notebookList(ctx context.Context, d notebookDeps, in []byte) (event.Payload, error) {
	tenant, ns, meta, into, ep, ok := notebookPrelude(ctx, d, in)
	if !ok {
		return ep, nil
	}
	r, err := d.store.List(ctx, tenant, ns,
		gjson.GetBytes(meta, "prefix").String(),
		gjson.GetBytes(meta, "after").String(),
		int(gjson.GetBytes(meta, "limit").Int()))
	if err != nil {
		return notebookErrFrom(into, err), nil
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for i, h := range r.Notebooks {
		if i > 0 {
			sb.WriteByte(',')
		}
		row := jsonx.NewObject()
		row.Set("name", h.Name)
		row.Set("high_seq", h.HighSeq)
		row.Set("ttl_secs", int64(h.TTL/time.Second))
		row.Set("created_at", chnotebook.FormatAt(h.CreatedAt))
		row.Set("updated_at", chnotebook.FormatAt(h.UpdatedAt))
		sb.WriteString(row.String())
	}
	sb.WriteByte(']')
	resp := jsonx.NewObject()
	resp.SetRaw(into+".notebooks", sb.String())
	resp.Set(into+".next", r.Next)
	resp.Set(into+".count", r.Count)
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// notebookDelete removes a notebook and its entries. Result at `into`:
// {deleted}. A later append recreates it under a new generation, so
// cursors taken before the delete stop working — loudly.
func notebookDelete(ctx context.Context, d notebookDeps, in []byte) (event.Payload, error) {
	tenant, ns, meta, into, ep, ok := notebookPrelude(ctx, d, in)
	if !ok {
		return ep, nil
	}
	ref := notebookRef(tenant, ns, meta)
	deleted, err := d.store.Delete(ctx, ref.Tenant, ref.Namespace, ref.Name)
	if err != nil {
		return notebookErrFrom(into, err), nil
	}
	resp := jsonx.NewObject()
	resp.Set(into+".deleted", deleted)
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// notebookSweeper reclaims expired entries on a ticker until ctx ends.
// Expired rows are already invisible to reads; this only frees space, so
// every node may run it — the batched DELETE is idempotent.
func notebookSweeper(ctx context.Context, logger *zap.Logger, st *chnotebook.Store, period time.Duration, batch int) {
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var total int64
		for {
			n, err := st.Sweep(ctx, time.Now(), batch)
			if err != nil {
				if ctx.Err() == nil {
					logger.Warn("notebook sweep failed", zap.String("err", err.Error()))
				}
				break
			}
			total += n
			if batch <= 0 || n < int64(batch) {
				break
			}
		}
		if total > 0 {
			logger.Info("notebook sweep reclaimed expired entries", zap.Int64("rows", total))
		}
	}
}
