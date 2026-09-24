package processor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/txcguard"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// outletDefaultInto is where the result lands when WITH into is absent.
// `_`-prefixed, so it never reaches a web body on its own.
const outletDefaultInto = "_outlet"

// ExecOutlet dispatches outlet://<name>/<op>: a statement written in the op
// against an external service the stack declared under OUTLETS/, with every
// value bound through `args`. The chassis holds the credential, owns the
// pool and bounds the call; the op sees rows.
//
// The result lands under `WITH into` (default `_outlet`):
//
//	<into>.ok             true | false
//	<into>.outlet         the outlet name
//	<into>.columns        SELECT order (query, or exec with RETURNING)
//	<into>.rows           [{col: val}, ...]
//	<into>.count          rows returned
//	<into>.rows_affected  exec only
//	<into>.error          {code, message[, sqlstate]} (only when not ok)
//
// Every failure — an undeclared outlet, a missing secret, a refused dial,
// a database error, a crossed ceiling, a timeout — is data: `ok = false`
// plus a coded error, and a nil Go error so the op still merges. The next
// op gates on `<into>.ok == true`; a missing path compares as false.
//
// outlet is a trusted transport (the handler builds the output), so the
// author-chosen `into` goes through txcguard.AuthorTarget: it may not name
// a reserved `_txc` path. The outlet is looked up for the dispatching op's
// own stack (op.Stack reduced to its app stack, as txco://kv does), never
// for a stack named in the envelope.
func (pu *Unit) ExecOutlet(ctx context.Context, op operation.Operation) (event.Payload, error) {
	into := outletDefaultInto
	if f := gjson.Get(op.Meta, "into"); f.Exists() {
		target, ok := txcguard.AuthorTarget(f.String())
		if !ok || target == "" {
			return outletPayload(op, into, "", "", outlet.Outcome{Err: outlet.NewError(outlet.CodeInvalidRequest, "WITH into must be a plain envelope path outside _txc")}), nil
		}
		into = target
	}
	name, kind, err := outlet.ParseRef(op.Resonator.Exec)
	if err != nil {
		return outletPayload(op, into, name, kind, outlet.Outcome{Err: outlet.NewError(outlet.CodeInvalidRequest, err.Error())}), nil
	}
	if pu.Outlets == nil {
		return outletPayload(op, into, name, kind, outlet.Outcome{Err: outlet.NewError(outlet.CodeUnavailable, "outlets are not configured on this node")}), nil
	}
	sqlField := gjson.Get(op.Meta, "sql")
	if sqlField.Type != gjson.String || sqlField.String() == "" {
		out := outlet.Outcome{Err: outlet.NewError(outlet.CodeInvalidRequest, "WITH sql must be a non-empty string literal")}
		emitOutletCompletionEvent(ctx, name, kind, "", out)
		return outletPayload(op, into, name, kind, out), nil
	}
	sql := sqlField.String()
	args, aerr := outlet.ConvertArgs(gjson.Get(op.Meta, "args"))
	if aerr != nil {
		out := outlet.Outcome{Err: aerr}
		emitOutletCompletionEvent(ctx, name, kind, sql, out)
		return outletPayload(op, into, name, kind, out), nil
	}

	out := pu.Outlets.Call(ctx, outlet.Call{
		Tenant: tenantScope(ctx),
		Stack:  workspace.AppStack(op.Stack),
		Outlet: name,
		Op:     kind,
		SQL:    sql,
		Args:   args,
	})
	if out.Result != nil {
		// Fuel per MiB returned, like notebook reads, on top of the flat
		// dispatch charge Exec already paid.
		if mib := (out.Result.Bytes + (1 << 20) - 1) >> 20; mib > 0 {
			_ = addFuel(ctx, mib*FuelCostOutletPerMiB, op.Stack+"/"+strconv.Itoa(op.Scope))
		}
	}
	emitOutletCompletionEvent(ctx, name, kind, sql, out)
	return outletPayload(op, into, name, kind, out), nil
}

// emitOutletCompletionEvent writes one outlet.completion TimelineEvent:
// outlet, driver, operation, a fingerprint of the statement, duration,
// rows, bytes and the error code. Never the SQL text, the argument values,
// row values or the DSN. The driver is carried because the URI hides it —
// fleet metrics want to aggregate one driver's behaviour without knowing
// what tenants named their outlets. On a ceiling error rows and bytes say
// how far the read got before it stopped.
func emitOutletCompletionEvent(ctx context.Context, name, kind, sql string, out outlet.Outcome) {
	tr := trace.FromContext(ctx)
	if tr == nil {
		return
	}
	fields := map[string]any{
		"outlet":      name,
		"operation":   kind,
		"duration_ms": int64(out.Duration / time.Millisecond),
	}
	if out.Driver != "" {
		fields["driver"] = out.Driver
	}
	if sql != "" {
		sum := sha256.Sum256([]byte(sql))
		fields["statement_sha256"] = hex.EncodeToString(sum[:8])
	}
	switch {
	case out.Result != nil:
		fields["rows"] = out.Result.Count
		fields["bytes"] = out.Result.Bytes
		if out.Result.RowsAffected > 0 {
			fields["rows_affected"] = out.Result.RowsAffected
		}
	case out.Err != nil:
		fields["error_code"] = out.Err.Code
		if out.Err.SQLState != "" {
			fields["sqlstate"] = out.Err.SQLState
		}
		if out.Err.Code == outlet.CodeResultTooLarge {
			fields["rows"] = out.Err.Rows
			fields["bytes"] = out.Err.Bytes
		}
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "outlet.completion", Fields: fields})
}

// outletPayload builds the op output: the result object set at `into`.
func outletPayload(op operation.Operation, into, name, kind string, out outlet.Outcome) event.Payload {
	result := string(buildOutletResult(name, kind, out))
	raw, err := sjson.SetRaw(`{}`, into, result)
	if err != nil {
		// into passed AuthorTarget, so this is unreachable in practice;
		// land at the default target rather than lose the result.
		raw = `{"` + outletDefaultInto + `":` + result + `}`
	}
	return event.Payload{Raw: raw, Type: event.JSON, Meta: op.Meta}
}

// buildOutletResult writes the result object by hand so the row array the
// driver produced is spliced in verbatim (no re-parse of a large result).
func buildOutletResult(name, kind string, out outlet.Outcome) []byte {
	var buf bytes.Buffer
	buf.WriteString(`{"ok":`)
	if out.Err == nil && out.Result != nil {
		r := out.Result
		buf.WriteString(`true,"outlet":`)
		writeJSONValue(&buf, name)
		if r.Columns != nil {
			buf.WriteString(`,"columns":`)
			writeJSONValue(&buf, r.Columns)
			buf.WriteString(`,"rows":`)
			if len(r.RowsJSON) == 0 {
				buf.WriteString("[]")
			} else {
				buf.Write(r.RowsJSON)
			}
			buf.WriteString(`,"count":`)
			writeJSONValue(&buf, r.Count)
		}
		if kind == outlet.OpExec {
			// exec reports rows_affected either way; a query never does.
			buf.WriteString(`,"rows_affected":`)
			writeJSONValue(&buf, r.RowsAffected)
		}
		buf.WriteByte('}')
		return buf.Bytes()
	}
	e := out.Err
	if e == nil {
		e = outlet.NewError(outlet.CodeUnavailable, "outlet returned no result")
	}
	buf.WriteString(`false`)
	if name != "" {
		buf.WriteString(`,"outlet":`)
		writeJSONValue(&buf, name)
	}
	buf.WriteString(`,"error":{"code":`)
	writeJSONValue(&buf, e.Code)
	buf.WriteString(`,"message":`)
	writeJSONValue(&buf, e.Message)
	if e.SQLState != "" {
		buf.WriteString(`,"sqlstate":`)
		writeJSONValue(&buf, e.SQLState)
	}
	buf.WriteString("}}")
	return buf.Bytes()
}
