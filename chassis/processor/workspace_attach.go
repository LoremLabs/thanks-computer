package processor

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/attach"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/usage"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// execWorkspaceAttach handles `workspace://<name>/attach`: it grants an
// interactive session (a PTY) to the WebSocket run that fired it and
// returns in milliseconds, leaving the process running behind an attached
// transport. From then on the websocket personality pumps frames to and
// from the process; the stack does not run per byte (D1 — an attachment is
// a granted capability, not a bypass).
//
// The refusals come first and in a fixed order, because the streaming work
// taught us a misordered gate surfaces the wrong error: not a websocket run
// → LOOP/stream → secrets → already bound → no provider.
func (pu *Unit) execWorkspaceAttach(ctx context.Context, op operation.Operation, spec workspace.Spec, name, into, prov string, refs []secrets.Ref, tenant, opID, rid string) (event.Payload, error) {
	sid := gjson.Get(op.Input, "_txc.websocket.session.id").String()
	if sourceScope(ctx) != "websocket" || sid == "" {
		return workspaceFailure(into, prov, "", "bad_request",
			"attach only applies inside a WebSocket session run; a stack accepts a socket, then its first message attaches", 0), nil
	}
	if op.Resonator != nil && op.Resonator.Loop != nil {
		return workspaceFailure(into, prov, "", "bad_request",
			"attach cannot be combined with LOOP: an attachment is granted once, not per pass", 0), nil
	}
	if gjson.Get(op.Meta, "stream").Bool() {
		return workspaceFailure(into, prov, "", "bad_request",
			"attach cannot be combined with stream: the attached transport carries the bytes, not the response body", 0), nil
	}
	if len(refs) > 0 {
		// D12: a PTY user can type a secret back out, and a value split across
		// frames cannot be redacted. The eventual answer is a credential
		// helper inside the workspace, not a token in an interactive shell.
		return workspaceFailure(into, prov, "", "bad_request",
			"attach refuses secrets: an interactive session could echo them back; use a workspace-side credential helper", 0), nil
	}
	if pu.Attachments == nil {
		return workspaceFailure(into, prov, "", "unsupported",
			"attach is not available on this node (no attachment registry)", 0), nil
	}
	if _, bound := pu.Attachments.Lookup(tenant, sid); bound {
		return workspaceFailure(into, prov, "", "bad_request",
			"this session already has an attachment; detach it first (a session holds one, and tmux multiplexes inside it)", 0), nil
	}

	// An attachment is always a terminal in v1, so force TTY on before the
	// geometry parse (cols/rows require it) — attach never passes `tty` in
	// its WITH clause.
	req, err := workspaceBaseRequest(op)
	if err != nil {
		return workspaceFailure(into, prov, "", "bad_request", err.Error(), 0), nil
	}
	req.TTY = true
	if err := parseWorkspaceGeometry(op, &req); err != nil {
		return workspaceFailure(into, prov, "", "bad_request", err.Error(), 0), nil
	}
	req.StdoutTo = nil

	dur, err := attachDuration(op, pu.Conf.WorkspaceAttachMaxDuration)
	if err != nil {
		return workspaceFailure(into, prov, "", "bad_request", err.Error(), 0), nil
	}

	now := time.Now().UTC()
	att := attach.New(ctx, attach.Params{
		ID:        hxid.New().String(),
		Tenant:    tenant,
		AppStack:  spec.Stack,
		Workspace: name,
		Kind:      attach.KindPTY,
		SessionID: sid,
		NodeID:    attachNodeID(pu),
		StartedAt: now,
		ExpiresAt: now.Add(dur),
	})

	// The session runs on the attachment's context, so ending the
	// attachment (detach, expiry, teardown, shutdown) kills the process.
	sess, h, runID, serr := pu.Workspaces.Start(att.Context(), spec, req)
	if serr != nil {
		att.Close("start failed")
		// No secrets on an attach (refused above), so nothing to scrub.
		return workspaceFailure(into, prov, "", workspaceErrorCode(serr), serr.Error(), 0), nil
	}
	att.Conn = attach.NewPTY(sess)
	att.RunID = runID
	att.Computer = h.Ref

	if err := pu.Attachments.Bind(att); err != nil {
		// Lost a race for this session; kill the just-started process.
		att.Close("bind race")
		return workspaceFailure(into, prov, h.Ref, "bad_request", err.Error(), 0), nil
	}

	if store := pu.Workspaces.Store(); store != nil {
		if lerr := store.InsertLease(ctx, workspace.Lease{
			ID: att.ID, Tenant: tenant, Stack: spec.Stack, Name: name,
			WorkspaceID: workspace.ID(tenant, spec.Stack, name), Kind: attach.KindPTY,
			SessionID: sid, NodeID: att.NodeID, RunID: runID,
			StartedAt: now, ExpiresAt: att.ExpiresAt, LastHeartbeat: now,
		}); lerr != nil {
			// The lease is bookkeeping; a failed insert must not strand a live
			// binding. Log-and-continue: the heartbeat will keep trying.
			pu.Logger.Warn("workspace attach: lease insert failed; binding is live but unmetered until the next heartbeat lands")
		}
	}

	// The heartbeat lives here, not in a background service: it needs the
	// usage sink and the store, and bgservice.Config has neither.
	pu.attachStart(att, spec)

	raw, _ := sjson.Set("{}", into+".attached", true)
	raw, _ = sjson.Set(raw, into+".run", runID)
	raw, _ = sjson.Set(raw, into+".workspace", name)
	raw, _ = sjson.Set(raw, into+".node", att.NodeID)
	raw, _ = sjson.Set(raw, into+".lease.id", att.ID)
	raw, _ = sjson.Set(raw, into+".lease.expires_at", att.ExpiresAt.Format(time.RFC3339))
	return event.Payload{Raw: workspaceStamp(raw, prov, h.Ref, runID, 0), Type: event.JSON}, nil
}

// attachDuration reads WITH max_duration (a duration string), defaulting to
// and clamped by the configured ceiling.
func attachDuration(op operation.Operation, ceilingStr string) (time.Duration, error) {
	ceiling, err := time.ParseDuration(ceilingStr)
	if err != nil || ceiling <= 0 {
		ceiling = 8 * time.Hour
	}
	v := gjson.Get(op.Meta, "max_duration")
	if !v.Exists() || v.Type == gjson.Null {
		return ceiling, nil
	}
	if v.Type != gjson.String {
		return 0, errors.New("WITH max_duration must be a duration string (e.g. \"8h\")")
	}
	d, perr := time.ParseDuration(v.String())
	if perr != nil {
		return 0, errors.New("WITH max_duration: " + perr.Error())
	}
	if d <= 0 {
		return 0, errors.New("WITH max_duration must be positive")
	}
	if d > ceiling {
		d = ceiling
	}
	return d, nil
}

// attachStart spawns the per-attachment heartbeat/metering loop and the
// expiry watch on the node holding the socket.
func (pu *Unit) attachStart(att *attach.Attachment, spec workspace.Spec) {
	wsID := workspace.ID(att.Tenant, spec.Stack, att.Workspace)
	opID := att.AppStack + "/attach/" + att.Workspace
	go func() {
		t := time.NewTicker(workspace.LeaseHeartbeatInterval)
		defer t.Stop()
		var prevIn, prevOut int64
		slice := func(final bool) {
			in, out := att.BytesIn.Load(), att.BytesOut.Load()
			pu.meterAttach(att, opID, in-prevIn, out-prevOut)
			prevIn, prevOut = in, out
			if store := pu.Workspaces.Store(); store != nil {
				bg := context.Background()
				now := time.Now().UTC()
				if final {
					_ = store.ReleaseLease(bg, att.ID, now)
					return
				}
				_ = store.Heartbeat(bg, att.ID, now)
				// A3: keep the live reaper off a workspace someone is sitting
				// in — it destroys on last_used_at alone until Phase 2 teaches
				// it about leases.
				_ = store.Touch(bg, wsID, now, att.RunID, workspace.StatusRunning)
			}
		}
		for {
			select {
			case <-att.Context().Done():
				slice(true)
				return
			case <-t.C:
				if att.Expired(time.Now().UTC()) {
					att.Close("expired")
					continue // the ctx.Done branch runs the final slice
				}
				slice(false)
			}
		}
	}()
}

// meterAttach emits one billable usage slice for an attachment. Unlike the
// in-request workspace event (a non-billable side line — the request itself
// carries the billable one), a lease has no request behind it, so its
// heartbeat IS the billing record: Billable, real fuel, the interval's
// duration. The precedent for a billable event on a context that outlives
// its request is llmgw.Gateway.fireCompletion. addFuel is deliberately not
// used: it reads its budget from the request context and silently no-ops
// without one.
func (pu *Unit) meterAttach(att *attach.Attachment, opID string, bytesIn, bytesOut int64) {
	if pu.Usage == nil {
		return
	}
	pu.Usage.WriteEvent(usage.UsageEvent{
		RID:        att.ID,
		Tenant:     att.Tenant,
		Src:        "workspace",
		Stack:      opID,
		DurationMS: workspace.LeaseHeartbeatInterval.Milliseconds(),
		Status:     "ok",
		BytesIn:    int(bytesIn),
		BytesOut:   int(bytesOut),
		Fuel:       workspaceFuel(workspace.LeaseHeartbeatInterval.Milliseconds()),
		Billable:   true,
	})
}

// attachNodeID names THIS chassis for the lease, so an operator elsewhere
// gets a real answer instead of a silent race (D5). Mirrors the server's
// resolveUsageNodeID (unexported there).
func attachNodeID(pu *Unit) string {
	if pu.Conf.Fqdn != "" {
		return pu.Conf.Fqdn
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "local"
}
