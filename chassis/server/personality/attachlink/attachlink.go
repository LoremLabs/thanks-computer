// Package attachlink is the machine's side of `txco attach`: a personality
// with no listener. It dials each chassis this machine is attached to,
// proves its key, and hands every request that comes down the link to the
// chassis's own `_attach` stack, as an inlet does (chassis/attached).
//
// It runs nothing itself. A request is an envelope into the bus; the stack
// decides what happens, and `_attach`'s built-in rule runs the command
// through the owner workspace provider. What goes back up the link is only
// the envelope's `attach` object, or its `workspace.error`.
//
// It also reports the end of detached jobs. A job started for an async op
// carries that op's continuation; when the job's exit is on disk the result
// goes up the link, on this connection or a later one, until the chassis
// acknowledges it.
//
// OFF unless `attachlink` is in --personalities and --attach-home is set.
// `txco attach` sets both; a full chassis may too.
package attachlink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/attached"
	"github.com/loremlabs/thanks-computer/chassis/attached/home"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
	"github.com/loremlabs/thanks-computer/chassis/workspace/detach"
)

const (
	// busWait bounds the handoff of a request to the bus.
	busWait = 10 * time.Second
	// endGrace is how long a request whose deadline has passed is given to
	// report its own end: the command is being killed, and its real result
	// is worth a moment more than a bare "timeout".
	endGrace = 5 * time.Second
	// jobPoll is how often ended jobs are looked for.
	jobPoll = time.Second
	// jobKeep is how long a finished job's directory is kept.
	jobKeep = 24 * time.Hour

	replyFile    = "reply.json"
	completeFile = "complete.json"
	doneFile     = "done.json"
)

// Controller runs one link per credential in the attach folder.
type Controller struct {
	ctx    context.Context
	cancel context.CancelFunc
	pu     *processor.Unit
	home   home.Home
	jobs   *detach.Store

	// Say, when set, prints one line for the owner (the terminal `txco
	// attach` runs in). The log gets every line regardless.
	Say func(string)

	// Idle, when set, is called once the last credential this process was
	// serving has been ended by its chassis: there is nothing left to do.
	Idle func()

	mu     sync.Mutex
	links  map[string]*attached.Link // by credential id
	served int
	logf   *os.File
	wg     sync.WaitGroup
}

// NewController constructs (but does not start) the personality.
func NewController(ctx context.Context, pu *processor.Unit) *Controller {
	cctx, cancel := context.WithCancel(ctx)
	return &Controller{ctx: cctx, cancel: cancel, pu: pu, links: map[string]*attached.Link{}}
}

func on(personalities, name string) bool {
	for _, p := range strings.Split(personalities, ",") {
		if strings.TrimSpace(p) == name {
			return true
		}
	}
	return false
}

// Start dials every live credential and begins watching for ended jobs.
func (c *Controller) Start() {
	if !on(c.pu.Conf.Personalities, "attachlink") {
		return
	}
	if c.pu.Conf.AttachHome == "" {
		c.pu.Logger.Info("attachlink personality enabled but no --attach-home; not started")
		return
	}
	c.home = home.Home{Dir: c.pu.Conf.AttachHome}
	if c.Say == nil && strings.TrimSpace(c.pu.Conf.Personalities) == "attachlink" {
		// `txco attach`: the terminal is the owner's, and these lines are
		// for them.
		c.Say = func(line string) { fmt.Fprintln(os.Stdout, line) }
	}
	jobs, err := detach.New(c.home.JobsDir(), nil)
	if err != nil {
		c.pu.Logger.Fatal("attachlink: jobs folder", zap.Error(err))
	}
	c.jobs = jobs
	if f, err := os.OpenFile(c.home.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		c.logf = f
	}
	creds, err := c.home.Load()
	if err != nil {
		c.pu.Logger.Fatal("attachlink: read credentials", zap.Error(err))
	}
	served := 0
	for _, cred := range creds {
		switch st, err := os.Stat(cred.Root); {
		case cred.Ended != "":
			c.say("not serving %s: the chassis ended this attachment (%s)", describe(cred), cred.Ended)
		case err != nil || !st.IsDir():
			// A folder that is gone is not served, and nothing is run
			// somewhere else instead. The name reads away.
			c.say("not serving %s: its folder is gone", describe(cred))
		default:
			served++
			c.mu.Lock()
			c.served++
			c.mu.Unlock()
			c.wg.Add(1)
			go c.serve(cred)
		}
	}
	if c.Idle == nil && strings.TrimSpace(c.pu.Conf.Personalities) == "attachlink" {
		// `txco attach` exists only to serve: with nothing left, it ends,
		// the way the chassis ends on a signal.
		c.Idle = func() {
			if p, err := os.FindProcess(os.Getpid()); err == nil {
				_ = p.Signal(syscall.SIGTERM)
			}
		}
	}
	if served == 0 {
		c.say("nothing to serve: no attached workspace in %s", c.home.Dir)
	}
	c.wg.Add(1)
	go c.watchJobs()
}

// Stop ends every link. Detached jobs are not this process's to end: they
// run on, and their ends are reported after the next start.
func (c *Controller) Stop() {
	c.cancel()
	c.wg.Wait()
	if c.logf != nil {
		_ = c.logf.Close()
	}
}

func describe(cred home.Credential) string {
	what := cred.Label
	if what == "" {
		what = cred.Name
	}
	return fmt.Sprintf("%s in %s", what, home.Pretty(cred.Root))
}

func (c *Controller) say(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	c.log(line)
	if c.Say != nil {
		c.Say(line)
	} else {
		c.pu.Logger.Info("attach: " + line)
	}
}

func (c *Controller) log(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.logf != nil {
		fmt.Fprintf(c.logf, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
	}
}

// serve keeps one credential's link up until the chassis says it is over.
func (c *Controller) serve(cred home.Credential) {
	defer c.wg.Done()
	machine, _ := os.Hostname()
	if cred.Machine != "" {
		machine = cred.Machine
	}
	link := &attached.Link{
		Credential: cred.ID, Key: cred.Key(),
		Machine: machine, Root: home.Pretty(cred.Root), OS: runtime.GOOS, Version: c.pu.Conf.Build.Version,
		Handler: &handler{c: c, cred: cred},
	}
	link.OnWelcome = func(attached.Welcome) { c.say("attached: %s", describe(cred)) }
	c.mu.Lock()
	c.links[cred.ID] = link
	c.mu.Unlock()

	for attempt := 0; ; attempt++ {
		started := time.Now()
		conn, host, err := attached.Dial(c.ctx, nil, cred.Address)
		if err == nil {
			link.Host = host
			err = link.Run(c.ctx, conn)
		}
		if c.ctx.Err() != nil {
			return
		}
		if attached.Final(err) {
			var ref *attached.Refused
			errors.As(err, &ref)
			// Replaced or revoked: this credential is finished. It is not
			// dialed again, so two machines with one key never trade a
			// name back and forth.
			_ = c.home.End(cred.ID, ref.Reason)
			if ref.Reason == attached.ReasonRevoked {
				c.dropJobs(cred.ID)
			}
			c.say("no longer serving %s: %s", describe(cred), ref.Reason)
			c.mu.Lock()
			c.served--
			left := c.served
			delete(c.links, cred.ID)
			c.mu.Unlock()
			if left == 0 && c.Idle != nil {
				c.say("nothing left to serve")
				c.Idle()
			}
			return
		}
		if time.Since(started) > time.Minute {
			attempt = 0
		}
		wait := time.Second << min(attempt, 6) // 1s … 64s
		wait += time.Duration(rand.Int64N(int64(wait)))
		c.log(fmt.Sprintf("link down credential=%s err=%v retry=%s", cred.ID, err, wait.Round(time.Millisecond)))
		select {
		case <-time.After(wait):
		case <-c.ctx.Done():
			return
		}
	}
}

// handler turns what comes down one credential's link into runs of the
// `_attach` stack.
type handler struct {
	c    *Controller
	cred home.Credential
}

func fail(code, msg string) attached.Reply {
	b, _ := json.Marshal(attached.ExecReply{Exit: -1, Error: &attached.WireError{Code: code, Message: msg}})
	return attached.Reply{OK: true, Payload: b}
}

// Request runs one request through `_attach` and returns its answer.
func (h *handler) Request(ctx context.Context, hdr attached.Header) attached.Reply {
	start := time.Now()
	var p attached.ExecPayload
	if err := json.Unmarshal(hdr.Payload, &p); err != nil {
		return attached.Reply{Error: &attached.WireError{Code: "bad_request", Message: "the request's payload is not an exec"}}
	}
	rep := h.run(ctx, hdr, p)
	h.c.log(fmt.Sprintf("request credential=%s id=%s detach=%v stdin=%d reply=%d ms=%d",
		h.cred.ID, hdr.ID, p.Detach, len(p.Stdin), len(rep.Payload), time.Since(start).Milliseconds()))
	return rep
}

func (h *handler) run(ctx context.Context, hdr attached.Header, p attached.ExecPayload) attached.Reply {
	rid := hxid.NewTimeSort().String()
	// The other chassis's request is DATA under _txc.attach.req. Every field
	// a rule reads is present, so a rule never meets a missing value.
	env := p.Env
	if env == nil {
		env = map[string]string{}
	}
	args := p.Args
	if args == nil {
		args = []string{}
	}
	req, _ := json.Marshal(map[string]any{
		"shell": p.Command != "", "command": p.Command, "args": args,
		"stdin": string(p.Stdin), "cwd": p.Cwd, "env": env,
		"timeout_ms": opTimeout(ctx, p), "detach": p.Detach,
	})
	payload := `{}`
	payload, _ = sjson.Set(payload, "_txc.src", "attach")
	payload, _ = sjson.Set(payload, "_txc.rid", rid)
	payload, _ = sjson.Set(payload, "_txc.attach.credential", h.cred.ID)
	payload, _ = sjson.Set(payload, "_txc.attach.id", hdr.ID)
	payload, _ = sjson.Set(payload, "_txc.attach.root", h.cred.Root)
	payload, _ = sjson.SetRaw(payload, "_txc.attach.req", string(req))
	payload, _ = sjson.Set(payload, "_ts", time.Now().UTC().Format(time.RFC3339))

	// The request's context is the run's: its deadline is the command's, and
	// a cancel from the chassis ends the command.
	runCtx := context.WithValue(ctx, config.CtxKeyRid, rid)
	resCh := make(chan event.Payload, 1)
	wait := time.NewTimer(busWait)
	defer wait.Stop()
	select {
	case h.c.pu.Bus <- event.PackageJSON(runCtx, payload, resCh, "attach"):
	case <-wait.C:
		// Nothing ran: the chassis may safely be told so.
		return attached.Reply{Error: &attached.WireError{Code: "unavailable", Message: "this machine is too busy to take the request"}}
	case <-ctx.Done():
		return fail("timeout", "the request ended before it could start")
	}
	var res event.Payload
	select {
	case res = <-resCh:
	case <-ctx.Done():
		select {
		case res = <-resCh:
		case <-time.After(endGrace):
			return fail("timeout", "the command did not end within its time")
		}
	}

	if e := gjson.Get(res.Raw, "workspace.error"); e.Exists() {
		code := e.Get("code").String()
		// The message as the stack saw it carries the error's own prefix;
		// the other chassis adds it again when it rebuilds the error.
		return fail(code, strings.TrimPrefix(e.Get("message").String(), "workspace: "+code+": "))
	}
	out := gjson.Get(res.Raw, "attach")
	if !out.IsObject() || (!out.Get("exit").Exists() && !out.Get("job").Exists()) {
		if ctx.Err() != nil {
			return fail("timeout", "the command did not end within its time")
		}
		// No rule ran the request: the stack's own answer, most often an
		// owner's policy declining it.
		return fail("not_allowed", "this machine's _attach stack ran nothing for the request")
	}
	er := attached.ExecReply{
		Exit:            int(out.Get("exit").Int()),
		Stdout:          out.Get("stdout").String(),
		Stderr:          out.Get("stderr").String(),
		StdoutTruncated: out.Get("stdout_truncated").Bool(),
		StderrTruncated: out.Get("stderr_truncated").Bool(),
		WallMS:          gjson.Get(res.Raw, "_txc.workspace.duration_ms").Int(),
		Job:             out.Get("job").String(),
	}
	if er.Job != "" && hdr.OPC != "" {
		// The job was started for an async op. Where its end must go is
		// written down BEFORE the chassis hears it started: a job's end is
		// reported only once this file exists.
		if err := h.c.writeReply(er.Job, pending{Credential: h.cred.ID, Request: hdr.ID, OPC: hdr.OPC, Token: hdr.Token}); err != nil {
			_ = h.c.jobs.Stop(er.Job)
			return fail("provider", "could not record where the job's end should go: "+err.Error())
		}
	}
	b, _ := json.Marshal(er)
	return attached.Reply{OK: true, Payload: b}
}

// opTimeout is the exec op's own timeout on this machine. A held command is
// given a little less than the request has left, so that it is the OP that
// runs out of time: the command is killed, the run ends in the ordinary way
// and says "timeout", all before the request's own deadline cuts the run off
// mid-air. A detached job's timeout is its whole life, and is passed as is.
func opTimeout(ctx context.Context, p attached.ExecPayload) int64 {
	if p.Detach {
		return p.TimeoutMS
	}
	left := p.TimeoutMS
	if d, ok := ctx.Deadline(); ok {
		if until := time.Until(d).Milliseconds(); left <= 0 || until < left {
			left = until
		}
	}
	margin := left / 5
	if margin > 1000 {
		margin = 1000
	}
	if left -= margin; left < 1 {
		left = 1
	}
	return left
}

// Stop asks a detached job to end.
func (h *handler) Stop(_ context.Context, job string) error {
	// Only a job started under this credential.
	if p, ok := h.c.readReply(job); ok && p.Credential != h.cred.ID {
		return &workspace.Error{Code: workspace.CodeNotFound, Message: "no such job"}
	}
	return h.c.jobs.Stop(job)
}

// pending says where a detached job's end goes: the continuation of the
// async op that started it. Mode 0600: the token is that op's capability.
type pending struct {
	Credential string `json:"credential"`
	Request    string `json:"request"`
	OPC        string `json:"opc"`
	Token      string `json:"token"`
}

func (c *Controller) writeReply(job string, p pending) error {
	dir, err := c.jobs.Dir(job)
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	return writeFile(dir, replyFile, raw)
}

func (c *Controller) readReply(job string) (pending, bool) {
	dir, err := c.jobs.Dir(job)
	if err != nil {
		return pending{}, false
	}
	var p pending
	raw, err := os.ReadFile(filepath.Join(dir, replyFile))
	if err != nil || json.Unmarshal(raw, &p) != nil || p.OPC == "" {
		return pending{}, false
	}
	return p, true
}

func writeFile(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpName, filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(tmpName)
	}
	return err
}

// watchJobs reports ended jobs and removes old ones, for as long as the
// personality runs. It starts with a scan, which is the whole of what a
// restart has to do: every state a job can be in is on disk.
//
//	a complete not yet acknowledged       send it
//	a reply and an exit, no complete      write the complete, then send it
//	a reply, no exit, wrapper alive       wait
//	a reply, no exit, wrapper gone        send "unknown": the machine cannot
//	                                      tell whether the command finished
//	no reply                              nothing to report
func (c *Controller) watchJobs() {
	defer c.wg.Done()
	tick := time.NewTicker(jobPoll)
	defer tick.Stop()
	for {
		c.scanJobs()
		select {
		case <-tick.C:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Controller) scanJobs() {
	ids, err := c.jobs.List()
	if err != nil {
		return
	}
	for _, id := range ids {
		dir, _ := c.jobs.Dir(id)
		if st, err := os.Stat(filepath.Join(dir, doneFile)); err == nil {
			if time.Since(st.ModTime()) > jobKeep {
				_ = c.jobs.Remove(id)
			}
			continue
		}
		p, ok := c.readReply(id)
		if !ok {
			// Nobody is waiting for this job's end. Keep it a day past its
			// end, for anyone who wants to read its output.
			if state, ex, _ := c.jobs.Status(id); state == detach.Ended && time.Since(ex.EndedAt) > jobKeep {
				_ = c.jobs.Remove(id)
			} else if state == detach.Lost {
				if st, err := os.Stat(dir); err == nil && time.Since(st.ModTime()) > jobKeep {
					_ = c.jobs.Remove(id)
				}
			}
			continue
		}
		complete, err := os.ReadFile(filepath.Join(dir, completeFile))
		if err != nil {
			if complete = c.resultOf(id); complete == nil {
				continue // still running
			}
			if err := writeFile(dir, completeFile, complete); err != nil {
				continue
			}
		}
		c.report(id, dir, p, complete)
	}
}

// resultOf builds an ended job's result in an exec's shape, or nil while
// the job is still running.
func (c *Controller) resultOf(id string) []byte {
	state, _, err := c.jobs.Status(id)
	if err != nil {
		return nil
	}
	var er attached.ExecReply
	switch state {
	case detach.Running:
		return nil
	case detach.Lost:
		er = attached.ExecReply{Exit: -1, Job: id, Error: &attached.WireError{Code: workspace.CodeUnknown,
			Message: "the machine restarted or the job's wrapper was killed before the command's end was recorded; it may or may not have finished"}}
	default:
		res, ex, err := c.jobs.Result(id, int64(c.pu.Conf.WorkspaceMaxOutputBytes))
		if err != nil {
			return nil
		}
		er = attached.ExecReply{Exit: res.Exit, Job: id, Stopped: ex.Stopped, WallMS: res.WallMS,
			Stdout: string(res.Stdout), StdoutTruncated: res.StdoutTruncated,
			Stderr: string(res.Stderr), StderrTruncated: res.StderrTruncated}
		if ex.TimedOut {
			er.Error = &attached.WireError{Code: "timeout", Message: "the command ran out of time"}
		}
	}
	b, _ := json.Marshal(er)
	return b
}

// report sends a job's end up its credential's link. Acknowledged: the
// chassis has recorded it durably, and this machine's copy can go. Refused
// with a code: the chassis will never take it (the op is gone, the
// credential revoked), so it is dropped. Anything else: kept, and sent
// again at the next scan.
func (c *Controller) report(id, dir string, p pending, complete []byte) {
	c.mu.Lock()
	link := c.links[p.Credential]
	c.mu.Unlock()
	if link == nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	err := link.Complete(ctx, attached.Header{OPC: p.OPC, Token: p.Token, Job: id, Payload: complete})
	var we *workspace.Error
	switch {
	case err == nil:
		c.finish(dir, "acknowledged")
		c.log(fmt.Sprintf("job reported job=%s bytes=%d", id, len(complete)))
	case errors.As(err, &we) && we.Code != "provider":
		c.finish(dir, "refused: "+we.Code)
		c.log(fmt.Sprintf("job report refused job=%s code=%s", id, we.Code))
	}
}

// finish marks a job as dealt with and forgets its continuation's token.
func (c *Controller) finish(dir, how string) {
	raw, _ := json.Marshal(map[string]string{"outcome": how, "at": time.Now().UTC().Format(time.RFC3339)})
	if writeFile(dir, doneFile, raw) == nil {
		_ = os.Remove(filepath.Join(dir, replyFile))
		_ = os.Remove(filepath.Join(dir, completeFile))
	}
}

// dropJobs ends a revoked credential's detached jobs. Their ends could no
// longer be received.
func (c *Controller) dropJobs(credential string) {
	ids, _ := c.jobs.List()
	for _, id := range ids {
		if p, ok := c.readReply(id); ok && p.Credential == credential {
			_ = c.jobs.Stop(id)
			if dir, err := c.jobs.Dir(id); err == nil {
				c.finish(dir, "credential revoked")
			}
		}
	}
}
