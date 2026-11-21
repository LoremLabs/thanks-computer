package attached

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
	"github.com/loremlabs/thanks-computer/chassis/workspace/detach"
)

// Service is attach as the rest of the chassis sees it: the answer for
// attached names (workspace.Attached), and the invite, status, revoke and
// stop behind the txco://attach ops.
type Service struct {
	Store  *Store
	Claims *Claims
	Hub    *Hub

	// Forward sends a header to the node that holds a link and returns its
	// reply: the cross-node hop. nil on a single node, where a link held
	// elsewhere cannot exist. It must answer CodeAway when the node is not
	// there to take it, and CodeUnknown when it took it and never replied.
	Forward func(ctx context.Context, node, workspaceID, conn string, gen int64, hdr Header) (Reply, error)

	// PublicURL is where machines dial this chassis, scheme and host. A
	// code is answered with its host appended, so `txco attach <code>`
	// knows where to go. Empty answers the code bare.
	PublicURL string

	now func() time.Time
}

// NewService wires the three parts for the node named node.
func NewService(store *Store, claims *Claims, node string) *Service {
	return &Service{Store: store, Claims: claims, Hub: NewHub(store, claims, node), now: time.Now}
}

var _ workspace.Attached = (*Service)(nil)

// Wake implements workspace.Attached. The Manager has already read the row:
// the name IS attached. Whether a machine is connected is this call's
// question, and no connected machine is CodeAway — never the provider.
func (s *Service) Wake(ctx context.Context, spec workspace.Spec, credential string) (workspace.Computer, error) {
	wid := workspace.ID(spec.Tenant, spec.Stack, spec.Name)
	claim, found, err := s.Claims.Read(ctx, spec.Tenant, wid)
	if err != nil {
		return nil, &workspace.Error{Code: "provider", Message: err.Error()}
	}
	if !found || !claim.Live(s.now()) {
		return nil, away("the workspace's machine is not connected")
	}
	return &computer{svc: s, tenant: spec.Tenant, wid: wid}, nil
}

// Release implements workspace.Attached: the workspace is being destroyed.
// The credential is revoked, the claim cleared (keeping its generation) and
// the link, wherever it is, closed. The Manager then marks the row
// destroyed, and the name is the provider's again.
func (s *Service) Release(ctx context.Context, spec workspace.Spec, credential string) error {
	wid := workspace.ID(spec.Tenant, spec.Stack, spec.Name)
	return s.end(ctx, spec.Tenant, wid)
}

// end revokes a workspace's credential and closes its link.
func (s *Service) end(ctx context.Context, tenant, wid string) error {
	if _, err := s.Store.Revoke(ctx, wid); err != nil {
		return &workspace.Error{Code: "provider", Message: err.Error()}
	}
	// The revoke is already in force: nothing more is forwarded. What
	// follows only tidies up sooner.
	claim, found, _ := s.Claims.Read(ctx, tenant, wid)
	if err := s.Claims.Clear(ctx, tenant, wid); err != nil {
		return &workspace.Error{Code: "provider", Message: err.Error()}
	}
	if found && !claim.Released {
		if claim.Node == s.Hub.Node() {
			s.Hub.Close(wid, claim.Conn, ReasonRevoked)
		} else if s.Hub.Notify != nil {
			go s.Hub.Notify(context.WithoutCancel(ctx), claim.Node, wid, claim.Conn, ReasonRevoked)
		}
	}
	return nil
}

// Invite makes a one-use code for the workspace.
func (s *Service) Invite(ctx context.Context, spec workspace.Spec, label string, ttl time.Duration) (code string, expires time.Time, err error) {
	code, expires, err = s.Store.CreateCode(ctx, spec, label, ttl)
	if err != nil {
		return "", time.Time{}, err
	}
	return code + AddressSuffix(s.PublicURL), expires, nil
}

// AddressSuffix is what an invite appends to a code: "@<host>" for an https
// address, "@<scheme>://<host>" for any other, and nothing for none.
func AddressSuffix(publicURL string) string {
	u, err := url.Parse(strings.TrimRight(publicURL, "/"))
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme == "https" {
		return "@" + u.Host
	}
	return "@" + u.Scheme + "://" + u.Host
}

// SplitCode separates what the owner pasted into the code and the address
// to dial. A bare host means https.
func SplitCode(pasted string) (code, base string) {
	code, addr, found := strings.Cut(strings.TrimSpace(pasted), "@")
	if !found || addr == "" {
		return code, ""
	}
	if !strings.Contains(addr, "://") {
		addr = "https://" + addr
	}
	return code, strings.TrimRight(addr, "/")
}

// Enroll spends code and makes e's key the workspace's one credential. An
// earlier credential for the name is revoked in the same transaction; its
// link, if it has one, is then closed. That closing is a courtesy: the old
// machine is already refused every request.
func (s *Service) Enroll(ctx context.Context, code string, e Enrollment) (*Credential, error) {
	cred, replaced, err := s.Store.Enroll(ctx, code, e)
	if err != nil {
		return nil, err
	}
	if len(replaced) > 0 {
		if claim, found, _ := s.Claims.Read(ctx, cred.Tenant, cred.WorkspaceID); found && !claim.Released {
			if claim.Node == s.Hub.Node() {
				s.Hub.Close(cred.WorkspaceID, claim.Conn, ReasonRevoked)
			} else if s.Hub.Notify != nil {
				go s.Hub.Notify(context.WithoutCancel(ctx), claim.Node, cred.WorkspaceID, claim.Conn, ReasonRevoked)
			}
		}
	}
	return cred, nil
}

// Revoke revokes the workspace's credential. The row stays attached, so the
// name reads away and is not handed back to the provider.
func (s *Service) Revoke(ctx context.Context, spec workspace.Spec) error {
	return s.end(ctx, spec.Tenant, workspace.ID(spec.Tenant, spec.Stack, spec.Name))
}

// Status is what txco://attach/status answers.
type Status struct {
	// Enrolled: the row says a machine holds the name (true after a revoke
	// too, until the workspace is destroyed).
	Enrolled bool `json:"enrolled"`
	// Attached: a machine is connected right now.
	Attached bool      `json:"attached"`
	Since    time.Time `json:"since,omitzero"`
	Machine  string    `json:"machine,omitempty"`
	Root     string    `json:"root,omitempty"`
	OS       string    `json:"os,omitempty"`
	Version  string    `json:"version,omitempty"`
}

// Status reports on one workspace. The DB says whether it is enrolled; the
// KV, only whether its machine is here now.
func (s *Service) Status(ctx context.Context, spec workspace.Spec) (Status, error) {
	enrolled, cred, err := s.Store.Enrolled(ctx, spec)
	if err != nil || !enrolled {
		return Status{}, err
	}
	st := Status{Enrolled: true}
	if cred != nil {
		st.Machine, st.Root, st.OS = cred.Machine, cred.Root, cred.OS
	}
	claim, found, err := s.Claims.Read(ctx, spec.Tenant, workspace.ID(spec.Tenant, spec.Stack, spec.Name))
	if err != nil {
		return st, err
	}
	if found && cred != nil && claim.Live(s.now()) {
		st.Attached, st.Since = true, claim.Since
		st.Machine, st.Root, st.OS, st.Version = claim.Machine, claim.Root, claim.OS, claim.Version
	}
	return st, nil
}

// Stop asks the machine to end a detached job.
func (s *Service) Stop(ctx context.Context, spec workspace.Spec, job string) error {
	rep, err := s.send(ctx, spec.Tenant, workspace.ID(spec.Tenant, spec.Stack, spec.Name), Header{Kind: KindStop, Job: job})
	if err != nil {
		return err
	}
	if !rep.OK {
		return rep.Error.err()
	}
	return nil
}

// send routes a header to the workspace's machine: this node's own link, or
// the node the claim names.
//
// A refusal that says the claim was stale is followed by ONE more read of
// the claim: the machine may have just reconnected, to this node or another.
// That is safe to do because a refusal means nothing was delivered.
func (s *Service) send(ctx context.Context, tenant, wid string, hdr Header) (Reply, error) {
	var lastGen int64
	for attempt := 0; ; attempt++ {
		claim, found, err := s.Claims.Read(ctx, tenant, wid)
		if err != nil {
			return Reply{}, &workspace.Error{Code: "provider", Message: err.Error()}
		}
		if !found || !claim.Live(s.now()) {
			return Reply{}, away("the workspace's machine is not connected")
		}
		if attempt > 0 && claim.Gen == lastGen {
			return Reply{}, away("the workspace's machine is not connected")
		}
		lastGen = claim.Gen
		var rep Reply
		switch {
		case claim.Node == s.Hub.Node():
			rep, err = s.Hub.Send(ctx, wid, claim.Conn, claim.Gen, hdr)
		case s.Forward != nil:
			rep, err = s.Forward(ctx, claim.Node, wid, claim.Conn, claim.Gen, hdr)
		default:
			err = away("the machine is connected to another node, and this chassis has no way to reach it")
		}
		if err != nil && isCode(err, workspace.CodeAway) && attempt == 0 {
			continue
		}
		return rep, err
	}
}

func isCode(err error, code string) bool {
	var we *workspace.Error
	return errors.As(err, &we) && we.Code == code
}

// computer is an attached workspace as the Manager drives it.
type computer struct {
	svc    *Service
	tenant string
	wid    string
}

// replyMargin is how much sooner than its caller the machine gives up, so
// that a command that simply runs out of time is reported as a timeout by
// the machine, in time for the caller to hear it — and only a reply that
// never comes is "unknown".
func replyMargin(remaining time.Duration) time.Duration {
	m := remaining / 10
	if m > 2*time.Second {
		m = 2 * time.Second
	}
	return m
}

// Exec runs req on the machine: one request down the link, one reply.
func (c *computer) Exec(ctx context.Context, req workspace.ExecRequest, lim workspace.Limits) (workspace.ExecResult, error) {
	fail := func(err error) (workspace.ExecResult, error) { return workspace.ExecResult{Exit: -1}, err }
	if req.TTY || req.StdoutTo != nil {
		return fail(&workspace.Error{Code: "unsupported", Message: "an attached workspace has no terminal and no streamed output yet"})
	}
	if req.Tree != nil {
		return fail(&workspace.Error{Code: workspace.CodeStackDirUnavailable, Message: "an attached workspace cannot place a stack tree"})
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return fail(&workspace.Error{Code: "bad_request", Message: "an exec on an attached workspace needs a deadline"})
	}
	remaining := time.Until(deadline)
	theirs := deadline.Add(-replyMargin(remaining))
	payload, err := json.Marshal(ExecPayload{
		Command: req.Command, Args: req.Args, Stdin: req.Stdin, Cwd: req.Cwd, Env: req.Env,
		TimeoutMS: time.Until(theirs).Milliseconds(),
	})
	if err != nil {
		return fail(&workspace.Error{Code: "bad_request", Message: err.Error()})
	}
	id, err := randomID("req_")
	if err != nil {
		return fail(&workspace.Error{Code: "provider", Message: err.Error()})
	}
	start := time.Now()
	rep, err := c.svc.send(ctx, c.tenant, c.wid, Header{Kind: KindRequest, ID: id, Deadline: theirs, Payload: payload})
	if err != nil {
		return fail(err)
	}
	res, err := resultOf(rep, lim)
	if res.WallMS == 0 {
		res.WallMS = time.Since(start).Milliseconds()
	}
	return res, err
}

// ExecAsync implements workspace.AsyncDispatcher: the command is started
// DETACHED on the machine, which is handed the op's continuation and sends
// the result for it when the command ends, on whatever connection it then
// has. Nothing is held open here.
func (c *computer) ExecAsync(ctx context.Context, req workspace.ExecRequest, lim workspace.Limits, t workspace.AsyncTicket) (string, workspace.ExecResult, bool, error) {
	fail := func(err error) (string, workspace.ExecResult, bool, error) {
		return "", workspace.ExecResult{Exit: -1}, false, err
	}
	if req.TTY || req.StdoutTo != nil || req.Tree != nil {
		return fail(&workspace.Error{Code: "unsupported", Message: "an async exec on an attached workspace has no terminal, no streamed output and no stack tree"})
	}
	if req.Timeout <= 0 {
		return fail(&workspace.Error{Code: "bad_request", Message: "an async exec needs the command's life (max_duration)"})
	}
	if t.OPC == "" || t.Token == "" {
		return fail(&workspace.Error{Code: "bad_request", Message: "an async exec needs its continuation"})
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return fail(&workspace.Error{Code: "bad_request", Message: "an async exec's handoff needs a deadline"})
	}
	payload, err := json.Marshal(ExecPayload{
		Command: req.Command, Args: req.Args, Stdin: req.Stdin, Cwd: req.Cwd, Env: req.Env,
		TimeoutMS: req.Timeout.Milliseconds(), Detach: true,
	})
	if err != nil {
		return fail(&workspace.Error{Code: "bad_request", Message: err.Error()})
	}
	id, err := randomID("req_")
	if err != nil {
		return fail(&workspace.Error{Code: "provider", Message: err.Error()})
	}
	rep, err := c.svc.send(ctx, c.tenant, c.wid, Header{
		Kind: KindRequest, ID: id, Deadline: deadline.Add(-replyMargin(time.Until(deadline))),
		OPC: t.OPC, Token: t.Token, Payload: payload,
	})
	if err != nil {
		return fail(err)
	}
	job, res, err := asyncReplyOf(rep, lim)
	return job, res, job != "", err
}

// asyncReplyOf reads the machine's answer to an async request: the job it
// started, or — when its own stack ran the command in place — the result.
func asyncReplyOf(rep Reply, lim workspace.Limits) (string, workspace.ExecResult, error) {
	if rep.OK {
		var er ExecReply
		if json.Unmarshal(rep.Payload, &er) == nil && er.Error == nil && er.Job != "" {
			if !detach.ValidID(er.Job) {
				return "", workspace.ExecResult{Exit: -1}, &workspace.Error{Code: "provider", Message: "the machine named a job this chassis cannot accept"}
			}
			return er.Job, workspace.ExecResult{}, nil
		}
	}
	res, err := resultOf(rep, lim)
	return "", res, err
}

// AsyncResultOf reads a completion's payload — a machine's word for how a
// detached job ended — into typed fields, cut to the chassis's own output
// caps. Nothing of the payload is kept as JSON: what a machine supplies is
// two strings, an exit code and a few flags.
func AsyncResultOf(raw json.RawMessage, lim workspace.Limits) (workspace.AsyncResult, error) {
	var er ExecReply
	if err := json.Unmarshal(raw, &er); err != nil {
		return workspace.AsyncResult{}, &workspace.Error{Code: "bad_request", Message: "the completion is not an exec result"}
	}
	out := workspace.AsyncResult{Job: er.Job, Stopped: er.Stopped}
	if er.Error != nil {
		out.ErrCode, out.ErrMessage = er.Error.Code, er.Error.Message
		out.Result = workspace.ExecResult{Exit: -1, WallMS: er.WallMS}
		return out, nil
	}
	b, _ := json.Marshal(er)
	res, err := resultOf(Reply{OK: true, Payload: b}, lim)
	out.Result = res
	return out, err
}

// resultOf turns a machine's reply into an exec's result. The machine's
// output is capped again here: this chassis does not take the machine's word
// for how much it sent.
func resultOf(rep Reply, lim workspace.Limits) (workspace.ExecResult, error) {
	if !rep.OK {
		if rep.Error == nil {
			return workspace.ExecResult{Exit: -1}, unknown("the machine refused the request without saying why")
		}
		return workspace.ExecResult{Exit: -1}, rep.Error.err()
	}
	var er ExecReply
	if err := json.Unmarshal(rep.Payload, &er); err != nil {
		return workspace.ExecResult{Exit: -1}, &workspace.Error{Code: "provider", Message: "the machine's reply was not an exec result"}
	}
	if er.Error != nil {
		if er.Error.Code == "timeout" {
			return workspace.ExecResult{Exit: -1, WallMS: er.WallMS}, workspace.ErrTimeout
		}
		return workspace.ExecResult{Exit: -1, WallMS: er.WallMS}, er.Error.err()
	}
	max := lim.MaxOutputBytes
	if max <= 0 {
		max = workspace.DefaultMaxOutputBytes
	}
	res := workspace.ExecResult{
		Exit: er.Exit, WallMS: er.WallMS,
		Stdout: []byte(er.Stdout), StdoutTruncated: er.StdoutTruncated,
		Stderr: []byte(er.Stderr), StderrTruncated: er.StderrTruncated,
	}
	if int64(len(res.Stdout)) > max {
		res.Stdout, res.StdoutTruncated = res.Stdout[:max], true
	}
	if int64(len(res.Stderr)) > max {
		res.Stderr, res.StderrTruncated = res.Stderr[:max], true
	}
	res.StdoutBytes = int64(len(res.Stdout))
	return res, nil
}
