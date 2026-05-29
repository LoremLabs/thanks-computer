package processor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
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
	if gjson.Get(op.Input, "_txc._debug").Bool() {
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

// ExecMCPHTTP dispatches an MCP-over-HTTP `tools/call` op. Mirrors
// ExecHTTP structurally and shares pu.HTTPClient so the egress
// guard, per-op context, and trace pipeline all apply for free.
//
// v0 is correct but not optimized: every op pays the full session
// lifecycle (initialize → notifications/initialized → tools/call).
// A session cache per (tenant, endpoint) is a v0.5 optimization
// that doesn't change this signature or the rule syntax.
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

	u, err := url.Parse(opName)
	if err != nil {
		return mcpFailPayload(op.Name, "url-parse", 0, err), err
	}
	tool := u.Fragment
	if tool == "" {
		err := fmt.Errorf("mcp+http: missing #tool-name fragment in %q", opName)
		return mcpFailPayload(op.Name, "missing-tool-fragment", 0, err), err
	}
	// Dispatch is by mcp+http:// or mcp+https://; underlying wire
	// scheme is the same string sans the mcp+ prefix.
	u.Scheme = strings.TrimPrefix(u.Scheme, "mcp+")
	u.Fragment = ""
	endpoint := u.String()
	if dbg != nil {
		dbg.Endpoint = endpoint
		dbg.Tool = tool
	}

	// Header-only secret overlays. Body overlays (`secrets.body.*`)
	// are silent no-ops in v0 — substituting cleartext into
	// `params.arguments` would expand the blast radius across the
	// wire, trace StepInfo.Output, run state, and async
	// continuation. Header auth (bearer tokens) covers the
	// dominant case without that exposure.
	headerOverlays := map[string]string{}
	if refs, perr := secrets.ParseRefs(op.Meta); perr != nil {
		return mcpFailPayload(op.Name, "secrets-parse", 0, perr), perr
	} else if len(refs) > 0 {
		var hdrOnly []secrets.Ref
		for _, r := range refs {
			if strings.HasPrefix(r.Path, "headers.") {
				hdrOnly = append(hdrOnly, r)
			}
		}
		if len(hdrOnly) > 0 {
			if _, aerr := applySecretOverlays(hdrOnly, op.Secrets, nil, headerOverlays); aerr != nil {
				return mcpFailPayload(op.Name, "secrets-overlay", 0, aerr), aerr
			}
		}
	}

	ua := "txco"
	if v := ctx.Value(config.CtxKeyVersion); v != nil {
		ua = fmt.Sprintf("txco/%s", v.(string))
	}

	// Build the tools/call request body once — used on both the
	// initial attempt and the one-shot post-eviction retry below.
	// `params.arguments` is the user's tool input — NOT the chassis
	// envelope. We extract it via this precedence:
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
	callParams, _ := sjson.SetRaw(`{}`, "name", jsonStringLit(tool))
	callParams, _ = sjson.SetRaw(callParams, "arguments", args)
	callReq, err := jsonrpc.Call(2, "tools/call", []byte(callParams))
	if err != nil {
		return mcpFailPayload(op.Name, "call-marshal", 0, err), err
	}

	// Session cache key — `tenant|endpoint`. nil cache (test default)
	// → cacheKey unused, lifecycle runs every call as before.
	cacheKey := sessionCacheKey(tenantScope(ctx), endpoint)

	// initLifecycle runs phases 1+2 (initialize → initialized) and
	// returns the server-minted session id. Called once on a cold
	// cache, and again on a one-shot retry after a session-invalidation
	// 4xx from tools/call. Returns the failure payload directly when
	// either phase errors — the caller doesn't need to wrap it.
	initLifecycle := func() (sid string, failPayload *event.Payload, failErr error) {
		initParams, _ := json.Marshal(map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "txco", "version": ua},
		})
		initReq, err := jsonrpc.Call(1, "initialize", initParams)
		if err != nil {
			fp := mcpFailPayload(op.Name, "init-marshal", 0, err)
			return "", &fp, err
		}
		initBody, gotSID, initStatus, err := pu.mcpPost(ctx, endpoint, initReq, "", ua, nil, "init", dbg)
		if err != nil {
			fp := mcpFailPayload(op.Name, "init", initStatus, err)
			return "", &fp, err
		}
		if _, perr := jsonrpc.Parse(initBody); perr != nil {
			fp := mcpFailPayload(op.Name, "init", initStatus, perr)
			return "", &fp, perr
		}

		notifBody, err := jsonrpc.Notify("notifications/initialized", nil)
		if err != nil {
			fp := mcpFailPayload(op.Name, "initialized-marshal", 0, err)
			return "", &fp, err
		}
		if _, _, ns, nerr := pu.mcpPost(ctx, endpoint, notifBody, gotSID, ua, nil, "initialized", dbg); nerr != nil {
			fp := mcpFailPayload(op.Name, "initialized", ns, nerr)
			return "", &fp, nerr
		}
		return gotSID, nil, nil
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
		newSID, fp, ierr := initLifecycle()
		if ierr != nil {
			return *fp, ierr
		}
		sessionID = newSID
		pu.MCPSessions.put(cacheKey, sessionID)
	}

	// Phase 3: tools/call — carries the real auth burden, so header
	// overlays apply here. If the server signals our session is gone
	// (4xx with "session" in the body), evict and retry once from
	// the top of the lifecycle. Other failures surface unchanged.
	callBody, newSID, callStatus, callErr := pu.mcpPost(ctx, endpoint, callReq, sessionID, ua, headerOverlays, "call", dbg)
	if callErr != nil && isSessionInvalidated(callStatus, callBody) {
		pu.MCPSessions.evict(cacheKey)
		if dbg != nil {
			dbg.SessionCache = "retry-reinit"
		}
		retrySID, fp, ierr := initLifecycle()
		if ierr != nil {
			return *fp, ierr
		}
		pu.MCPSessions.put(cacheKey, retrySID)
		sessionID = retrySID
		callBody, newSID, callStatus, callErr = pu.mcpPost(ctx, endpoint, callReq, sessionID, ua, headerOverlays, "call", dbg)
	}
	if callErr != nil {
		return mcpFailPayload(op.Name, "call", callStatus, callErr), callErr
	}

	// Some servers rotate the session id on tools/call. If so, refresh
	// the cache with the new sid so subsequent calls don't echo the
	// old one back.
	if newSID != "" && newSID != sessionID {
		pu.MCPSessions.put(cacheKey, newSID)
	}

	resp, perr := jsonrpc.Parse(callBody)
	if perr != nil {
		return mcpFailPayload(op.Name, "call", callStatus, perr), perr
	}
	payload, perr = projectMCPResult(resp.Result)
	if perr == nil {
		// _txc._ops.<opname> = {status, message} so downstream rules
		// can branch on op-level success/failure. See mcpFailPayload
		// for the failure-path shape.
		ok, _ := sjson.Set(`{}`, "status", callStatus)
		ok, _ = sjson.Set(ok, "message", "ok")
		payload.Raw, _ = sjson.SetRaw(payload.Raw, "_txc._ops."+opsKey(op.Name), ok)
	}
	return payload, perr
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
// response body and any session id the server returned. The shared
// pu.HTTPClient enforces egress policy at dial time.
func (pu *Unit) mcpPost(ctx context.Context, endpoint string, body []byte, sessionID, ua string, overlays map[string]string, phase string, dbg *mcpDebugLog) ([]byte, string, int, error) {
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

	resp, err := pu.HTTPClient.Do(req)
	if err != nil {
		return nil, "", 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
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
// envelope. v0 sugar:
//   - result.content = [{type:text,text:S}]  → {"text": S}
//   - result.content = [block,...]            → {"content": [...]}
//   - no content field                        → result verbatim
//   - result missing/empty                    → "{}"
func projectMCPResult(result json.RawMessage) (event.Payload, error) {
	if len(result) == 0 {
		return event.NewJSON("{}").CreateJSONPayload()
	}
	content := gjson.GetBytes(result, "content")
	if !content.Exists() {
		return event.NewJSON(string(result)).CreateJSONPayload()
	}
	if content.IsArray() {
		arr := content.Array()
		if len(arr) == 1 && arr[0].Get("type").String() == "text" {
			out := `{}`
			out, _ = sjson.Set(out, "text", arr[0].Get("text").String())
			return event.NewJSON(out).CreateJSONPayload()
		}
	}
	// Multiple blocks, non-text-only, or non-array content — pass
	// through; the rule unwraps.
	out := `{}`
	out, _ = sjson.SetRaw(out, "content", content.Raw)
	return event.NewJSON(out).CreateJSONPayload()
}

// mcpFailPayload builds the failure-shape event.Payload used when
// the MCP lifecycle fails at any phase. Two things land:
//   - `_txc._ops.<opname>` in Raw — visible to downstream rules so
//     they can branch on op-level outcome (status, message, error,
//     phase). `_txc.*` is stripped at the web outlet, so this is
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
// is reachable from rules; dotted names would otherwise be parsed
// as nested paths by sjson.
func opsKey(name string) string {
	if name == "" {
		return "_anonymous"
	}
	return name
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
