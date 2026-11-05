package processor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/connection"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonrpc"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// Debug payload cap. Opt-in only — see mcpDebugEnabled.
const mcpDebugBodyPreviewMax = 512

type mcpPhaseLog struct {
	Phase                string `json:"phase"`
	DurationMs           int64  `json:"duration_ms"`
	HTTPStatus           int    `json:"http_status"`
	ReqBodyBytes         int    `json:"req_body_bytes"`
	ReqBodyPreview       string `json:"req_body_preview,omitempty"`
	RespContentType      string `json:"response_content_type,omitempty"`
	RespBodyBytes        int    `json:"response_body_bytes"`
	RespBodyPreview      string `json:"response_body_preview,omitempty"`
	SSEUnwrapAttempted   bool   `json:"sse_unwrap_attempted,omitempty"`
	SSEMessageFrameFound bool   `json:"sse_message_frame_found,omitempty"`
}

type mcpDebugLog struct {
	Endpoint     string        `json:"endpoint,omitempty"`
	Tool         string        `json:"tool,omitempty"`
	SessionCache string        `json:"session_cache,omitempty"` // "hit" | "miss" | "retry-reinit"
	Phases       []mcpPhaseLog `json:"phases,omitempty"`
}

func mcpDebugEnabled(op operation.Operation) bool {
	if gjson.Get(op.Meta, "debug").Bool() {
		return true
	}
	if gjson.Get(op.EnvelopeView(), "_txc._debug").Bool() {
		return true
	}
	return false
}

func clipForPreview(b []byte) string {
	if len(b) <= mcpDebugBodyPreviewMax {
		return string(b)
	}
	return string(b[:mcpDebugBodyPreviewMax])
}

func attachMCPDebug(payload event.Payload, opName string, dbg *mcpDebugLog) event.Payload {
	if dbg == nil {
		return payload
	}
	b, err := json.Marshal(dbg)
	if err != nil {
		return payload
	}
	raw := payload.Raw
	if raw == "" {
		raw = "{}"
	}
	if next, serr := sjson.SetRawBytes([]byte(raw), "_txc._ops."+opsKey(opName)+"._debug", b); serr == nil {
		payload.Raw = string(next)
	}
	return payload
}

// mcpProtocolVersion is the spec version advertised on `initialize`.
// Pin here so a spec bump is one line.
const mcpProtocolVersion = "2025-06-18"

const mcpSessionHeader = "Mcp-Session-Id"

// Bounds on one `tools/list`: how many tools are kept and how many pages
// are asked for. A server with more answers `truncated: true`.
const (
	mcpToolsListMax   = 200
	mcpToolsListPages = 20
)

// ExecMCPHTTP dispatches an MCP-over-HTTP op: `tools/call` (the default)
// or `tools/list` (`WITH method = "tools/list"`). Mirrors ExecHTTP
// structurally and shares pu.HTTPClient so the egress guard, per-op
// context, and trace pipeline all apply for free.
//
// The literal EXEC names the server and the tool:
//
//	EXEC "mcp+https://host/path#tool"
//
// and a WITH clause may supply either from data, so one rule can call a
// server and a tool the envelope names:
//
//	WITH url       = ._call.url     // http(s)://… or mcp+http(s)://…; a #fragment names the tool
//	     tool      = ._call.tool    // wins over any #fragment
//	     args_path = "_call.args"   // the one envelope value sent as `arguments`
//	     into      = "_res"         // nest the answer; a failure becomes data (below)
//	     method    = "tools/list"   // list the server's tools instead of calling one
//
// The override is scheme-locked to http(s), as ExecHTTP's is, so it cannot
// leave the egress-guarded client.
//
// Credentials ride every phase of the session (initialize,
// notifications/initialized, and the call): a server that requires
// authorization refuses `initialize` without it. A tenant secret
// (`secrets.headers.<h>.secret`) or a principal's connection
// (`secrets.headers.<h>.connection`, chassis/connection) sets a header, and
// sessions are cached per (tenant, endpoint, credential), so two principals
// at one server never share a session id.
//
// Failures. Without `WITH into`, a failed phase is a Go error and the op's
// output is dropped (the long-standing behavior). With `WITH into`, a
// runtime failure — the dial, an HTTP status, a JSON-RPC error, an answer
// over --op-payload-max — is DATA at
// `<into>.error.{code,message,phase,status,rpc_code}` and the op succeeds,
// as a txco:// op's failure and a connection's are, so a rule can say what
// went wrong. Authoring errors (a bad URL, no tool, an args_path that names
// nothing) stay Go errors either way.
//
// The session lifecycle (initialize → notifications/initialized) runs on a
// cold cache; a cached session id skips it.
func (pu *Unit) ExecMCPHTTP(ctx context.Context, op operation.Operation) (payload event.Payload, err error) {
	opName := op.Resonator.Exec
	pu.Logger.Debug("ExecMCPHTTP", zap.String("opname", opName), zap.String("input", op.Input))

	var dbg *mcpDebugLog
	if mcpDebugEnabled(op) {
		dbg = &mcpDebugLog{}
	}
	defer func() {
		payload = attachMCPDebug(payload, op.Name, dbg)
	}()

	// One scan of op.Meta for every WITH field this handler consults.
	metaFields := gjson.GetMany(op.Meta, "url", "tool", "args_path", "method", "into")

	// `WITH url` names the endpoint from data. EXEC stays a literal so the
	// scheme dispatches here; the override may be written with or without
	// the `mcp+` prefix, and must be http(s) underneath.
	target := opName
	if override := strings.TrimSpace(metaFields[0].String()); override != "" {
		plain := strings.TrimPrefix(override, "mcp+")
		if !strings.HasPrefix(plain, "http://") && !strings.HasPrefix(plain, "https://") {
			oerr := fmt.Errorf("mcp+http: WITH url override must be http(s) or mcp+http(s), got %q", override)
			return mcpFailPayload(op.Name, "url-override-scheme", 0, oerr), oerr
		}
		target = plain
	}
	u, err := url.Parse(target)
	if err != nil {
		return mcpFailPayload(op.Name, "url-parse", 0, err), err
	}
	// Dispatch is by mcp+http:// or mcp+https://; underlying wire
	// scheme is the same string sans the mcp+ prefix.
	u.Scheme = strings.TrimPrefix(u.Scheme, "mcp+")
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		uerr := fmt.Errorf("mcp+http: %q is not an http(s) endpoint", target)
		return mcpFailPayload(op.Name, "url-parse", 0, uerr), uerr
	}
	tool := u.Fragment
	u.Fragment = ""
	if t := strings.TrimSpace(metaFields[1].String()); t != "" {
		tool = t
	}
	method := strings.TrimSpace(metaFields[3].String())
	if method == "" {
		method = "tools/call"
	}
	if method != "tools/call" && method != "tools/list" {
		merr := fmt.Errorf("mcp+http: WITH method must be tools/call or tools/list, got %q", method)
		return mcpFailPayload(op.Name, "method", 0, merr), merr
	}
	if method == "tools/call" && tool == "" {
		terr := fmt.Errorf("mcp+http: missing #tool-name fragment in %q (or WITH tool)", opName)
		return mcpFailPayload(op.Name, "missing-tool-fragment", 0, terr), terr
	}
	endpoint := u.String()
	into := boundedInto(metaFields[4].String())
	if dbg != nil {
		dbg.Endpoint = endpoint
		dbg.Tool = tool
	}

	// The call's arguments, before anything is sent: an args_path that
	// names nothing is an authoring error, not a failed call.
	// `params.arguments` is the user's tool input — NOT the chassis
	// envelope. `WITH args_path` names the one envelope value to send;
	// without it we extract by this precedence:
	//   1. _txc.web.req.body (base64-decoded as JSON) — the natural
	//      "POST body becomes the MCP arguments" path. Web inlets
	//      base64 the request body to preserve binary content; for
	//      MCP we decode it and pass the JSON through.
	//   2. op.Input with _txc and _ts stripped — for non-web inlets
	//      (cron, mcp-ingress, etc.) and for rules that explicitly
	//      built up arguments in the envelope root via EMIT/SET.
	//   3. op.Input verbatim as a last resort (legacy / tests).
	// This keeps chassis plumbing (_txc.*, _ts) out of third-party
	// tool input by default, which matters for strict-schema servers
	// like DeepWiki that pydantic-validate their arguments.
	args := extractMCPArguments(op.Input)
	if ap := normalizeEnvelopePath(metaFields[2].String()); ap != "" {
		v := gjson.Get(op.Input, ap)
		if !v.Exists() {
			aerr := fmt.Errorf("mcp+http: WITH args_path %q names nothing in the envelope", ap)
			return mcpFailPayload(op.Name, "args-path-missing", 0, aerr), aerr
		}
		if !v.IsObject() {
			aerr := fmt.Errorf("mcp+http: WITH args_path %q is not an object (a tool's arguments are one)", ap)
			return mcpFailPayload(op.Name, "args-path-not-object", 0, aerr), aerr
		}
		args = v.Raw
	}

	// Header credentials. Body overlays (`secrets.body.*`) are silent
	// no-ops — substituting cleartext into `params.arguments` would
	// expand the blast radius across the wire, trace StepInfo.Output,
	// run state, and async continuation. A header covers bearer auth
	// without that exposure, and it rides every phase: the handshake
	// goes to the same endpoint as the call.
	headerOverlays := map[string]string{}
	client := pu.HTTPClient
	var credParts []string
	if refs, perr := secrets.ParseRefs(op.Meta); perr != nil {
		return mcpFailPayload(op.Name, "secrets-parse", 0, perr), perr
	} else if len(refs) > 0 {
		secretRefs, connRefs := secrets.ConnectionRefs(refs)
		var hdrOnly []secrets.Ref
		for _, r := range secretRefs {
			if strings.HasPrefix(r.Path, "headers.") {
				hdrOnly = append(hdrOnly, r)
				credParts = append(credParts, "s:"+r.Secret)
			}
		}
		if len(hdrOnly) > 0 {
			if _, aerr := applySecretOverlays(hdrOnly, op.Secrets, nil, headerOverlays); aerr != nil {
				return mcpFailPayload(op.Name, "secrets-overlay", 0, aerr), aerr
			}
		}
		if len(connRefs) > 0 {
			// A connection ref the chassis will not honour is data, not a
			// failed op, exactly as on https (ExecHTTP): the rule reads
			// `<into>.error.code`. Nothing is sent in that case.
			if pu.Connections == nil {
				return connectionFailure(into, connRefs[0].Connection, &connection.Error{
					Code: connection.CodeUnavailable, Message: "connections are not configured on this node (no identity store, or no master key)"}), nil
			}
			principal, capability := CapAuthorityFrom(ctx)
			for _, r := range connRefs {
				name := strings.TrimPrefix(r.Path, "headers.")
				if !strings.HasPrefix(r.Path, "headers.") || name == "" {
					return connectionFailure(into, r.Connection, &connection.Error{
						Code: connection.CodeInvalidArg, Message: fmt.Sprintf("secrets.%s.connection: a connection's token goes on a header (secrets.headers.<name>.connection)", r.Path)}), nil
				}
				res, cerr := pu.Connections.Use(ctx, connection.UseInput{
					Tenant: tenantScope(ctx), Stack: op.Stack, Provider: r.Connection,
					Principal: principal, Capability: capability, URL: u,
				})
				if cerr != nil {
					pu.Logger.Info("ExecMCPHTTP connection not used", zap.String("stack", op.Stack), zap.String("provider", r.Connection), zap.String("code", cerr.Code))
					return connectionFailure(into, r.Connection, cerr), nil
				}
				value := res.Value
				if r.Format != "" {
					// The op's own format wins over the consumer's declared one.
					v, ferr := secrets.Substitute(r.Format, []byte(res.Token))
					if ferr != nil {
						return connectionFailure(into, r.Connection, &connection.Error{Code: connection.CodeInvalidArg, Message: ferr.Error()}), nil
					}
					value = v
				}
				headerOverlays[name] = value
				credParts = append(credParts, "c:"+r.Connection+":"+principal)
			}
			// Guarded at the dial whatever the node's policy, following no
			// redirect: the client a token-carrying request is made with.
			client = pu.Connections.HTTPClient(u)
		}
	}
	sort.Strings(credParts)

	ua := "txco"
	if v := ctx.Value(config.CtxKeyVersion); v != nil {
		ua = fmt.Sprintf("txco/%s", v.(string))
	}

	// fail answers a runtime failure: data under `into` when the rule
	// named one, else the Go error it has always been.
	fail := func(phase string, status int, e error) (event.Payload, error) {
		if into != "" {
			return mcpFailureData(ctx, into, phase, status, e), nil
		}
		return mcpFailPayload(op.Name, phase, status, e), e
	}

	// Session cache key — `tenant|endpoint`, and the credential when the
	// call carries one. nil cache (test default) → cacheKey unused,
	// lifecycle runs every call as before.
	cacheKey := sessionCacheKeyFor(tenantScope(ctx), endpoint, strings.Join(credParts, ","))

	// initLifecycle runs phases 1+2 (initialize → initialized) and
	// returns the server-minted session id. Called once on a cold
	// cache, and again on a one-shot retry after a session-invalidation
	// 4xx. On failure it names the phase and the HTTP status.
	initLifecycle := func() (sid, phase string, status int, ierr error) {
		initParams, _ := json.Marshal(map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "txco", "version": ua},
		})
		initReq, merr := jsonrpc.Call(1, "initialize", initParams)
		if merr != nil {
			return "", "init-marshal", 0, merr
		}
		initBody, gotSID, initStatus, perr := pu.mcpPost(ctx, client, endpoint, initReq, "", ua, headerOverlays, "init", dbg)
		if perr != nil {
			return "", "init", initStatus, perr
		}
		if _, perr := jsonrpc.Parse(initBody); perr != nil {
			return "", "init", initStatus, perr
		}

		notifBody, merr := jsonrpc.Notify("notifications/initialized", nil)
		if merr != nil {
			return "", "initialized-marshal", 0, merr
		}
		if _, _, ns, nerr := pu.mcpPost(ctx, client, endpoint, notifBody, gotSID, ua, headerOverlays, "initialized", dbg); nerr != nil {
			return "", "initialized", ns, nerr
		}
		return gotSID, "", 0, nil
	}

	// Pull a cached session if available; otherwise run init.
	sessionID, hit := pu.MCPSessions.get(cacheKey)
	if dbg != nil {
		if hit {
			dbg.SessionCache = "hit"
		} else {
			dbg.SessionCache = "miss"
		}
	}
	if !hit {
		newSID, phase, st, ierr := initLifecycle()
		if ierr != nil {
			return fail(phase, st, ierr)
		}
		sessionID = newSID
		pu.MCPSessions.put(cacheKey, sessionID)
	}

	// rpc posts one request on the session. If the server signals our
	// session is gone (4xx with "session" in the body), evict and retry
	// once from the top of the lifecycle — once per op, however many
	// requests it makes. Other failures surface unchanged, with the phase
	// that failed.
	retried := false
	rpc := func(reqBody []byte, phase string) ([]byte, int, string, error) {
		body, newSID, status, rerr := pu.mcpPost(ctx, client, endpoint, reqBody, sessionID, ua, headerOverlays, phase, dbg)
		if rerr != nil && !retried && isSessionInvalidated(status, body) {
			retried = true
			pu.MCPSessions.evict(cacheKey)
			if dbg != nil {
				dbg.SessionCache = "retry-reinit"
			}
			retrySID, iphase, ist, ierr := initLifecycle()
			if ierr != nil {
				return nil, ist, iphase, ierr
			}
			pu.MCPSessions.put(cacheKey, retrySID)
			sessionID = retrySID
			body, newSID, status, rerr = pu.mcpPost(ctx, client, endpoint, reqBody, sessionID, ua, headerOverlays, phase, dbg)
		}
		if rerr != nil {
			return body, status, phase, rerr
		}
		// Some servers rotate the session id on a request. If so, refresh
		// the cache with the new sid so later requests don't echo the old
		// one back.
		if newSID != "" && newSID != sessionID {
			pu.MCPSessions.put(cacheKey, newSID)
			sessionID = newSID
		}
		return body, status, phase, nil
	}

	if method == "tools/list" {
		out, lphase, lstatus, lerr := pu.mcpListTools(rpc)
		if lerr != nil {
			return fail(lphase, lstatus, lerr)
		}
		return mcpAnswer(op.Name, into, out, lstatus, false)
	}

	// tools/call.
	callParams, _ := sjson.SetRaw(`{}`, "name", jsonStringLit(tool))
	callParams, _ = sjson.SetRaw(callParams, "arguments", args)
	callReq, err := jsonrpc.Call(2, "tools/call", []byte(callParams))
	if err != nil {
		return mcpFailPayload(op.Name, "call-marshal", 0, err), err
	}
	callBody, callStatus, cphase, callErr := rpc(callReq, "call")
	if callErr != nil {
		return fail(cphase, callStatus, callErr)
	}
	resp, perr := jsonrpc.Parse(callBody)
	if perr != nil {
		return fail("call", callStatus, perr)
	}
	// A tool that reports its own failure (`isError`) still answered: that
	// is the result's to say, not a failed op.
	return mcpAnswer(op.Name, into, projectMCPResultJSON(resp.Result), callStatus, gjson.GetBytes(resp.Result, "isError").Bool())
}

// mcpListTools pages `tools/list` over rpc until the server names no next
// cursor, keeping at most mcpToolsListMax tools, mcpToolsListPages pages and
// --op-payload-max bytes of them. Each tool is kept as the server wrote it
// (name, description, inputSchema, annotations, …). Answers
// `{"tools":[…]}`, with `truncated: true` when the server had more. On
// failure it names the phase and the HTTP status.
func (pu *Unit) mcpListTools(rpc func(reqBody []byte, phase string) ([]byte, int, string, error)) (out, phase string, status int, err error) {
	limit := pu.Conf.OpPayloadMax
	if limit <= 0 {
		limit = 4 << 20
	}
	tools := make([]string, 0, 32)
	total := 0
	cursor := ""
	truncated := false
	for page := 0; ; page++ {
		var params []byte
		if cursor != "" {
			params, _ = json.Marshal(map[string]string{"cursor": cursor})
		}
		req, merr := jsonrpc.Call(2+page, "tools/list", params)
		if merr != nil {
			return "", "list-marshal", 0, merr
		}
		body, st, rphase, rerr := rpc(req, "list")
		if rerr != nil {
			return "", rphase, st, rerr
		}
		status = st
		resp, perr := jsonrpc.Parse(body)
		if perr != nil {
			return "", "list", st, perr
		}
		if arr := gjson.GetBytes(resp.Result, "tools"); arr.IsArray() {
			for _, t := range arr.Array() {
				if len(tools) >= mcpToolsListMax {
					truncated = true
					break
				}
				total += len(t.Raw)
				if total > limit {
					return "", "list", st, fmt.Errorf("%w (%d bytes)", errOpAnswerTooLarge, limit)
				}
				tools = append(tools, t.Raw)
			}
		}
		cursor = gjson.GetBytes(resp.Result, "nextCursor").String()
		if truncated || cursor == "" {
			break
		}
		if page+1 >= mcpToolsListPages {
			truncated = true
			break
		}
	}
	out = `{"tools":[` + strings.Join(tools, ",") + `]}`
	if truncated {
		out, _ = sjson.Set(out, "truncated", true)
	}
	return out, "", status, nil
}

// mcpAnswer is a successful op's payload: the projected answer, nested
// under `into` when the rule named one, and `_txc._ops.<opname>` =
// {status, message[, is_error]} so downstream rules can branch on the
// op-level outcome. See mcpFailPayload for the failure-path shape.
func mcpAnswer(opName, into, out string, status int, isError bool) (event.Payload, error) {
	if into != "" {
		if wrapped, werr := sjson.SetRaw("{}", into, out); werr == nil {
			out = wrapped
		}
	}
	payload, err := event.NewJSON(out).CreateJSONPayload()
	if err != nil {
		return payload, err
	}
	ok, _ := sjson.Set(`{}`, "status", status)
	ok, _ = sjson.Set(ok, "message", "ok")
	if isError {
		ok, _ = sjson.Set(ok, "is_error", true)
	}
	payload.Raw, _ = sjson.SetRaw(payload.Raw, "_txc._ops."+opsKey(opName), ok)
	return payload, nil
}

// mcpFailureMessageMax bounds the message a failure carries as data: it may
// quote the server's own words.
const mcpFailureMessageMax = 300

// mcpFailureData is a runtime failure as data under `into` (`WITH into`):
//
//	<into>.error.code     txco_mcp_dial | _timeout | _http | _rpc | _too_large | _bad_answer
//	<into>.error.message  what failed, at most 300 characters
//	<into>.error.phase    init | initialized | call | list
//	<into>.error.status   the HTTP status, when there was an answer
//	<into>.error.rpc_code the JSON-RPC error code, for txco_mcp_rpc
//
// Never a Go error: the rule reads the code and says what to do.
func mcpFailureData(ctx context.Context, into, phase string, status int, e error) event.Payload {
	code := "txco_mcp_bad_answer" // an answer that is not JSON-RPC
	var rpcErr *jsonrpc.Error
	switch {
	case errors.Is(e, errOpAnswerTooLarge):
		code = "txco_mcp_too_large"
	case errors.As(e, &rpcErr):
		code = "txco_mcp_rpc"
	case status >= 400:
		code = "txco_mcp_http"
	case errors.Is(e, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = "txco_mcp_timeout"
	case status == 0:
		code = "txco_mcp_dial" // no HTTP answer at all
	}
	msg := ""
	if e != nil {
		msg = e.Error()
	}
	if len(msg) > mcpFailureMessageMax {
		msg = msg[:mcpFailureMessageMax]
	}
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	raw, _ = sjson.Set(raw, into+".error.phase", phase)
	if status > 0 {
		raw, _ = sjson.Set(raw, into+".error.status", status)
	}
	if code == "txco_mcp_rpc" && rpcErr != nil {
		raw, _ = sjson.Set(raw, into+".error.rpc_code", rpcErr.Code)
	}
	return event.Payload{Raw: raw, Type: event.JSON}
}

// isSessionInvalidated reports whether a tools/call failure shape
// indicates the server has dropped/lost our cached session id. MCP
// spec doesn't standardize the exact signal; we treat any 4xx whose
// response body contains "session" (case-insensitive) as session-
// related. Conservative — a false positive costs one cache eviction
// + one re-init, a true positive unsticks the caller. Doesn't fire
// on 5xx (server-broken, retrying same path won't help) or on
// network errors (callBody is empty so the substring miss is
// correct).
func isSessionInvalidated(status int, body []byte) bool {
	if status < 400 || status >= 500 {
		return false
	}
	return bytes.Contains(bytes.ToLower(body), []byte("session"))
}

// mcpPost POSTs body to endpoint with MCP / JSON content negotiation,
// the session id if set, and any header overlays. Returns the
// response body and any session id the server returned. client is
// pu.HTTPClient (egress policy at dial time), or the connection
// service's when the request carries a connection's token.
func (pu *Unit) mcpPost(ctx context.Context, client *http.Client, endpoint string, body []byte, sessionID, ua string, overlays map[string]string, phase string, dbg *mcpDebugLog) ([]byte, string, int, error) {
	start := time.Now()
	var phaseLog *mcpPhaseLog
	if dbg != nil {
		dbg.Phases = append(dbg.Phases, mcpPhaseLog{
			Phase:          phase,
			ReqBodyBytes:   len(body),
			ReqBodyPreview: clipForPreview(body),
		})
		phaseLog = &dbg.Phases[len(dbg.Phases)-1]
		defer func() { phaseLog.DurationMs = time.Since(start).Milliseconds() }()
	}

	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewBuffer(body))
	if err != nil {
		return nil, "", 0, err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/json")
	// Streamable-HTTP servers can respond with either application/
	// json or text/event-stream; per the MCP spec the client MUST
	// accept both. Servers that find one missing return 406 (e.g.
	// DeepWiki: "Client must accept both application/json and
	// text/event-stream").
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set(mcpSessionHeader, sessionID)
	}
	// Overlays last so an operator's secret-bound header (e.g.
	// Authorization) wins over chassis defaults.
	for k, v := range overlays {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	// Bounded by --op-payload-max like every op answer; a read that fails
	// part way keeps what arrived, as it always has.
	respBody, rerr := readOpAnswer(resp.Body, pu.Conf.OpPayloadMax)
	if errors.Is(rerr, errOpAnswerTooLarge) {
		return nil, "", resp.StatusCode, rerr
	}
	status := resp.StatusCode

	sid := resp.Header.Get(mcpSessionHeader)
	if sid == "" && sessionID != "" {
		// Some servers only emit the header on the initialize
		// response. Propagate the caller's session id through.
		sid = sessionID
	}

	// Capture raw response details before SSE unwrap so the preview
	// reflects what the server actually sent on the wire.
	if phaseLog != nil {
		phaseLog.HTTPStatus = status
		phaseLog.RespContentType = resp.Header.Get("Content-Type")
		phaseLog.RespBodyBytes = len(respBody)
		phaseLog.RespBodyPreview = clipForPreview(respBody)
	}

	// Unwrap SSE framing when the server chose event-stream.
	// Streamable-HTTP one-shot responses arrive as a single
	// `event: message` frame whose `data:` carries the JSON-RPC
	// payload; subsequent callers expect bare JSON. Long-lived
	// streams are out of scope.
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		if phaseLog != nil {
			phaseLog.SSEUnwrapAttempted = true
		}
		if extracted := jsonrpc.ExtractSSE(respBody); extracted != nil {
			respBody = extracted
			if phaseLog != nil {
				phaseLog.SSEMessageFrameFound = true
			}
		}
	}

	if status >= 400 {
		// 4xx commonly means "missing or unknown session id" or
		// "unsupported protocol version" — surface the body so the
		// trace tells the author what the server actually said.
		snippet := string(respBody)
		if len(snippet) > 200 {
			snippet = snippet[:200] + "..."
		}
		return respBody, sid, status, fmt.Errorf("mcp+http %d: %s", status, snippet)
	}
	return respBody, sid, status, nil
}

// projectMCPResult shapes a JSON-RPC `result` value into the
// envelope:
//   - result.content = [{type:text,text:S}]  → {"text": S}
//   - result.content = [block,...]            → {"content": [...]}
//   - no content field                        → result verbatim
//   - result missing/empty                    → "{}"
//
// Beside `text` or `content`, a result's `structuredContent` is kept under
// that name, and `isError: true` when the tool said it failed.
func projectMCPResult(result json.RawMessage) (event.Payload, error) {
	return event.NewJSON(projectMCPResultJSON(result)).CreateJSONPayload()
}

// projectMCPResultJSON is projectMCPResult's JSON, before it is nested
// under `into` or made a payload.
func projectMCPResultJSON(result json.RawMessage) string {
	if len(result) == 0 {
		return "{}"
	}
	content := gjson.GetBytes(result, "content")
	if !content.Exists() {
		return string(result)
	}
	out := `{}`
	single := false
	if content.IsArray() {
		arr := content.Array()
		if len(arr) == 1 && arr[0].Get("type").String() == "text" {
			out, _ = sjson.Set(out, "text", arr[0].Get("text").String())
			single = true
		}
	}
	if !single {
		// Multiple blocks, non-text-only, or non-array content — pass
		// through; the rule unwraps.
		out, _ = sjson.SetRaw(out, "content", content.Raw)
	}
	if sc := gjson.GetBytes(result, "structuredContent"); sc.Exists() {
		out, _ = sjson.SetRaw(out, "structuredContent", sc.Raw)
	}
	if gjson.GetBytes(result, "isError").Bool() {
		out, _ = sjson.Set(out, "isError", true)
	}
	return out
}

// mcpFailPayload builds the failure-shape event.Payload used when
// the MCP lifecycle fails at any phase. Two things land:
//   - `_txc._ops.<opname>` in Raw — visible to downstream rules so
//     they can branch on op-level outcome (status, message, error,
//     phase). `_txc.*` is stripped by the web response writer, so this is
//     chassis-internal metadata, not a user-facing leak.
//   - meta.error[] / meta.errorMsg — trace-only fields (unchanged
//     from prior behavior; the chassis doesn't merge Meta into the
//     envelope).
func mcpFailPayload(opName, phase string, status int, err error) event.Payload {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	ops, _ := sjson.Set(`{}`, "status", status)
	ops, _ = sjson.Set(ops, "message", msg)
	ops, _ = sjson.Set(ops, "error", "mcp-http-"+phase)
	ops, _ = sjson.Set(ops, "phase", phase)

	raw, _ := sjson.SetRaw(`{}`, "_txc._ops."+opsKey(opName), ops)

	meta, _ := sjson.Set(``, "error.0", "mcp-http-"+phase)
	meta, _ = sjson.Set(meta, "errorMsg", msg)
	return event.Payload{
		Raw:  raw,
		Type: event.JSON,
		Meta: meta,
	}
}

// opsKey makes an op's `name` usable as a JSON object key inside
// `_txc._ops`. Empty names fall back to "_anonymous" so the value
// is reachable from rules; sjsonKey escapes path metacharacters and
// forces an object key, so a name in digits is not an array index.
func opsKey(name string) string {
	if name == "" {
		return "_anonymous"
	}
	return sjsonKey(name)
}

// jsonStringLit returns the JSON-encoded literal for a string,
// safe to pass to sjson.SetRaw as a value.
func jsonStringLit(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// extractMCPArguments projects the op input into the JSON that
// becomes `tools/call.params.arguments`. See ExecMCPHTTP's phase-3
// comment for the precedence rules. Returns a valid JSON value
// (always); empty / unparseable input becomes "{}".
func extractMCPArguments(envelope string) string {
	if envelope == "" {
		return "{}"
	}
	// Path 1: web inlet — decode the base64 request body.
	if b64 := gjson.Get(envelope, "_txc.web.req.body"); b64.Exists() {
		if decoded, err := base64.StdEncoding.DecodeString(b64.String()); err == nil {
			s := string(decoded)
			if s == "" {
				return "{}"
			}
			// Verify it's parseable JSON so the downstream
			// JSON-RPC marshaller doesn't choke; fall through to
			// envelope-strip if not.
			if gjson.Valid(s) {
				return s
			}
		}
	}
	// Path 2: strip chassis plumbing keys and use the remaining
	// envelope as arguments. Covers chained-EXEC pipelines that
	// built up app data in the envelope root.
	stripped := envelope
	for _, k := range []string{"_txc", "_ts"} {
		if gjson.Get(stripped, k).Exists() {
			if next, err := sjson.Delete(stripped, k); err == nil {
				stripped = next
			}
		}
	}
	// If anything's left, use it.
	if gjson.Valid(stripped) && len(gjson.Parse(stripped).Map()) > 0 {
		return stripped
	}
	// Path 3: nothing extractable — empty arguments.
	return "{}"
}
