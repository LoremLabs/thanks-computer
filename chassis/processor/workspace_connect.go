package processor

import (
	"context"
	"fmt"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/attach"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// execWorkspaceConnect handles `workspace://<name>/connect`: it binds a byte
// stream to one of the workspace's own loopback services — named, never
// addressed — for the WebSocket run that fired it, and returns in
// milliseconds. From then on the websocket personality pumps frames to and
// from the service exactly as it does for a PTY; the stack does not run
// per byte. It is attach's second Conn (todo-workspace-browser-takeover.md
// D1–D4): same refusal ladder, same lease, same metering, same teardown.
//
// `WITH service` is the whole vocabulary. The chassis resolves the name
// against workspace.Services — the built-in table plus operator entries
// from --workspace-services — so no WITH value, and no model output that
// reached one, can name a host or a port. An unknown name is a bad_request
// that lists the ones this node knows.
func (pu *Unit) execWorkspaceConnect(ctx context.Context, op operation.Operation, spec workspace.Spec, name, into, prov string, refs []secrets.Ref, tenant, opID, rid string) (event.Payload, error) {
	sid := gjson.Get(op.Input, "_txc.websocket.session.id").String()
	if sourceScope(ctx) != "websocket" || sid == "" {
		return workspaceFailure(into, prov, "", "bad_request",
			"connect only applies inside a WebSocket session run; a stack accepts a socket, then its first message connects", 0), nil
	}
	if op.Resonator != nil && op.Resonator.Loop != nil {
		return workspaceFailure(into, prov, "", "bad_request",
			"connect cannot be combined with LOOP: a binding is granted once, not per pass", 0), nil
	}
	if gjson.Get(op.Meta, "stream").Bool() {
		return workspaceFailure(into, prov, "", "bad_request",
			"connect cannot be combined with stream: the attached transport carries the bytes, not the response body", 0), nil
	}
	if len(refs) > 0 {
		// Same reason as attach (D12): bytes on this transport cannot be
		// redacted, and a person can read a framebuffer as easily as a
		// terminal.
		return workspaceFailure(into, prov, "", "bad_request",
			"connect refuses secrets: the bound service could echo them back; use a workspace-side credential helper", 0), nil
	}
	if pu.Attachments == nil {
		return workspaceFailure(into, prov, "", "unsupported",
			"connect is not available on this node (no attachment registry)", 0), nil
	}
	if _, bound := pu.Attachments.Lookup(tenant, sid); bound {
		return workspaceFailure(into, prov, "", "bad_request",
			"this session already has an attachment; detach it first (a session holds one)", 0), nil
	}

	table := pu.Workspaces.Services()
	v := gjson.Get(op.Meta, "service")
	if !v.Exists() || v.Type != gjson.String || v.String() == "" {
		return workspaceFailure(into, prov, "", "bad_request",
			fmt.Sprintf("WITH service is required: one of %v", table.Names()), 0), nil
	}
	svc, ok := table.Lookup(v.String())
	if !ok {
		return workspaceFailure(into, prov, "", "bad_request",
			fmt.Sprintf("unknown service %q; this node knows %v", v.String(), table.Names()), 0), nil
	}

	dur, err := attachDuration(op, pu.Conf.WorkspaceConnectMaxDuration)
	if err != nil {
		return workspaceFailure(into, prov, "", "bad_request", err.Error(), 0), nil
	}

	now := time.Now().UTC()
	att := attach.New(ctx, attach.Params{
		ID:        hxid.New().String(),
		Tenant:    tenant,
		AppStack:  spec.Stack,
		Workspace: name,
		Kind:      attach.KindService,
		Service:   svc.Name,
		SessionID: sid,
		NodeID:    attachNodeID(pu),
		StartedAt: now,
		ExpiresAt: now.Add(dur),
	})

	// The connection lives on the attachment's context, so ending the
	// attachment (detach, expiry, teardown, shutdown) closes it.
	conn, h, runID, derr := pu.Workspaces.Dial(att.Context(), spec, svc)
	if derr != nil {
		att.Close("dial failed")
		return workspaceFailure(into, prov, "", workspaceErrorCode(derr), derr.Error(), 0), nil
	}
	att.Conn = attach.NewNet(conn)
	att.RunID = runID
	att.Computer = h.Ref

	if err := pu.Attachments.Bind(att); err != nil {
		// Lost a race for this session; drop the just-opened connection.
		att.Close("bind race")
		return workspaceFailure(into, prov, h.Ref, "bad_request", err.Error(), 0), nil
	}

	if store := pu.Workspaces.Store(); store != nil {
		if lerr := store.InsertLease(ctx, workspace.Lease{
			ID: att.ID, Tenant: tenant, Stack: spec.Stack, Name: name,
			WorkspaceID: workspace.ID(tenant, spec.Stack, name), Kind: attach.KindService,
			SessionID: sid, NodeID: att.NodeID, RunID: runID,
			StartedAt: now, ExpiresAt: att.ExpiresAt, LastHeartbeat: now,
		}); lerr != nil {
			pu.Logger.Warn("workspace connect: lease insert failed; binding is live but unmetered until the next heartbeat lands")
		}
	}

	pu.attachStart(att, spec)

	raw, _ := sjson.Set("{}", into+".connected", true)
	raw, _ = sjson.Set(raw, into+".service", svc.Name)
	raw, _ = sjson.Set(raw, into+".run", runID)
	raw, _ = sjson.Set(raw, into+".workspace", name)
	raw, _ = sjson.Set(raw, into+".node", att.NodeID)
	raw, _ = sjson.Set(raw, into+".lease.id", att.ID)
	raw, _ = sjson.Set(raw, into+".lease.expires_at", att.ExpiresAt.Format(time.RFC3339))
	return event.Payload{Raw: workspaceStamp(raw, prov, h.Ref, runID, 0), Type: event.JSON}, nil
}
