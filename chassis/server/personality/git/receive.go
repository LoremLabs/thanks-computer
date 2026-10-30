package git

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	gitcore "github.com/loremlabs/thanks-computer/chassis/git"
)

// Change is one file a push changes, as the lane hears it [G1]: the path,
// the blob, its size and mode, what happened to it, and two facts about
// its content a stack judges a binary by (docs/0061 §8): whether it is
// text, and its media type as sniffed from its first bytes.
type Change struct {
	Path string
	Hash plumbing.Hash
	Size int64
	Mode filemode.FileMode
	// Op is add, modify or delete.
	Op string
	// Text is whether the blob is UTF-8 without a NUL (false for a delete).
	Text bool
	// Type is the media type http.DetectContentType reads from the first 512
	// bytes ("" for a delete): image/png, application/pdf, text/plain;
	// charset=utf-8, application/octet-stream when nothing is recognized.
	Type string
}

// Push is one receive-pack request after its objects are staged: what the
// lane is asked about and what the refs are moved from.
type Push struct {
	Tenant   string
	Repo     string
	Who      authn.Authenticated
	ClientIP string
	// Commands are the client's ref updates, in its order.
	Commands []*packp.Command
	// Changes are the files each command changes, old tree → new tree.
	Changes map[plumbing.ReferenceName][]Change

	staging      Staging
	reachable    []plumbing.Hash
	refused      map[plumbing.ReferenceName]string
	unpack       string
	commits      []envCommit
	commitsTotal int
}

func (p *Push) refuse(name plumbing.ReferenceName, why string) {
	if _, done := p.refused[name]; !done {
		p.refused[name] = oneLine(why)
	}
}

// pending counts the commands not yet refused.
func (p *Push) pending() int {
	n := 0
	for _, cmd := range p.Commands {
		if _, no := p.refused[cmd.Name]; !no {
			n++
		}
	}
	return n
}

// refuseRest refuses every command not yet refused with why.
func (p *Push) refuseRest(why string) {
	for _, cmd := range p.Commands {
		p.refuse(cmd.Name, why)
	}
}

const (
	reasonAtomic = "atomic push failure"
	reasonUnpack = "unpack failed"
)

// serveReceivePack is a push. In order: the probe a large push opens with;
// the commands, each checked (only refs/heads/*, a valid name, never the
// default branch's deletion); the pack, parsed into a staging area that
// resolves deltas against the repository and refuses past the caps; a
// reachability walk from every new tip (a pack is self-consistent but may
// omit objects; what the walk finds is exactly what is kept); the files
// each command changes (text only, regular mode, under the object cap);
// the objects kept; the response HEADERS flushed, so a front proxy's
// time-to-headers never fires while the lane runs; the hook (the answer
// lane, G1); then the refs, all or none, by compare-and-swap; then the
// report, one line per ref, which git prints to the developer.
func (c *Controller) serveReceivePack(w http.ResponseWriter, r *http.Request, repo Repo, req request) {
	ctx := r.Context()
	body, err := c.requestBody(w, r, c.limits.MaxPackBytes)
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	br := bufio.NewReader(body)
	// The probe: before a body over http.postBuffer (1 MiB), git POSTs
	// exactly one flush to learn the status and the authentication, then
	// streams the real request chunked. Answer it empty.
	if head, perr := br.Peek(4); perr == nil && bytes.Equal(head, pktline4Flush) {
		_, _ = io.Copy(io.Discard, br)
		c.resultHeaders(w)
		w.WriteHeader(http.StatusOK)
		return
	}
	ur := packp.NewReferenceUpdateRequest()
	if err := ur.Decode(br); err != nil {
		_, _ = io.Copy(io.Discard, br)
		http.Error(w, "malformed receive-pack request", http.StatusBadRequest)
		return
	}
	p := &Push{
		Tenant: req.tenant, Repo: req.name, Who: req.who, ClientIP: req.ip,
		Commands: ur.Commands,
		Changes:  map[plumbing.ReferenceName][]Change{},
		refused:  map[plumbing.ReferenceName]string{},
		unpack:   "ok",
		staging:  repo.Stage(),
	}
	refs, err := repo.Refs(ctx)
	if err != nil {
		_, _ = io.Copy(io.Discard, br)
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	tips := make([]plumbing.Hash, 0, len(refs))
	for _, ref := range refs {
		tips = append(tips, ref.Hash)
	}

	// 1. The commands.
	wantsPack := false
	for _, cmd := range p.Commands {
		if why := checkCommand(repo, cmd); why != "" {
			p.refuse(cmd.Name, why)
		}
		if !cmd.New.IsZero() {
			wantsPack = true
		}
	}
	if len(p.Commands) == 0 {
		p.unpack = "no commands"
	}

	// 2. The pack, into the staging.
	pack := ur.Packfile
	if pack == nil {
		pack = io.NopCloser(br)
	}
	if wantsPack && p.unpack == "ok" {
		p.unpack = c.unpack(pack, p.staging)
	}
	// The client finishes sending before it reads a reply: drain.
	_, _ = io.Copy(io.Discard, pack)
	_, _ = io.Copy(io.Discard, br)

	// 3. Reachability, then what each command changes.
	if p.unpack == "ok" {
		for _, cmd := range p.Commands {
			if _, no := p.refused[cmd.Name]; no || cmd.New.IsZero() {
				continue
			}
			objs, err := revlistObjects(p.staging, cmd.New, tips)
			if err != nil {
				p.refuse(cmd.Name, "missing necessary objects")
				continue
			}
			p.reachable = append(p.reachable, objs...)
			changes, why := c.inspect(p.staging, repo.Meta(), cmd)
			if why != "" {
				p.refuse(cmd.Name, why)
				continue
			}
			p.Changes[cmd.Name] = changes
		}
	}
	// A push is one unit: anything refused refuses the rest.
	if p.unpack != "ok" {
		p.refuseRest(reasonUnpack)
	} else if len(p.refused) > 0 {
		p.refuseRest(reasonAtomic)
	}
	if p.pending() > 0 {
		describeCommits(p)
	}

	// 4. Keep the objects the moved refs reach.
	if p.pending() > 0 && len(p.reachable) > 0 {
		if err := p.staging.Commit(ctx, dedupe(p.reachable)); err != nil {
			switch {
			case errors.Is(err, gitcore.ErrRepoTooLarge):
				p.unpack = fmt.Sprintf("repository over its cap of %d bytes", c.limits.MaxRepoBytes)
			case errors.Is(err, gitcore.ErrObjectTooLarge):
				p.unpack = fmt.Sprintf("an object is over %d bytes", c.limits.MaxObjectBytes)
			default:
				c.logger().Warn("git push: objects not stored", zap.String("tenant", p.Tenant), zap.String("repo", p.Repo), zap.Error(err))
				p.unpack = "could not store the objects"
			}
			p.refuseRest(reasonUnpack)
		}
	}

	// 5. Headers out before anything slow.
	c.resultHeaders(w)
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()

	// 6. The answer lane: the tenant's `_git` stack judges the push before
	// any ref moves (policy `stack`); a test hook may stand in for it.
	if p.pending() > 0 {
		switch {
		case c.beforeRefs != nil:
			if err := c.beforeRefs(ctx, p); err != nil {
				p.refuseRest(err.Error())
			}
		case repo.Meta().PolicyPush != "open":
			if a := c.lanes.ask(p); !a.ok {
				p.refuseRest(a.msg)
			}
		}
	}

	// 7. The refs, all or none.
	moved := false
	if p.pending() > 0 {
		updates := make([]RefUpdate, 0, len(p.Commands))
		for _, cmd := range p.Commands {
			updates = append(updates, RefUpdate{Name: cmd.Name, Old: cmd.Old, New: cmd.New})
		}
		actor := Actor{Principal: p.Who.Principal.ID, Credential: p.Who.Credential, Source: "push"}
		if err := repo.UpdateRefs(ctx, updates, actor); err != nil {
			var mv *ErrRefMoved
			if errors.As(err, &mv) {
				p.refuse(mv.Name, fmt.Sprintf("ref moved: expected %s, is %s", shortHash(oldOf(p.Commands, mv.Name)), shortHash(mv.Current)))
			} else {
				c.logger().Warn("git push: refs not updated", zap.String("tenant", p.Tenant), zap.String("repo", p.Repo), zap.Error(err))
			}
			p.refuseRest(reasonAtomic)
		} else {
			moved = true
		}
	}

	// 8. The report.
	if ur.Capabilities != nil && ur.Capabilities.Supports(capability.ReportStatus) {
		rs := packp.NewReportStatus()
		rs.UnpackStatus = p.unpack
		for _, cmd := range p.Commands {
			status := "ok"
			if why, no := p.refused[cmd.Name]; no {
				status = why
			}
			rs.CommandStatuses = append(rs.CommandStatuses, &packp.CommandStatus{ReferenceName: cmd.Name, Status: status})
		}
		if err := rs.Encode(w); err != nil {
			c.logger().Debug("git push: report not written", zap.Error(err))
		}
	}
	// 9. The observe lane: an accepted push is told to the stack after the
	// reply, fire-and-forget.
	if moved {
		c.lanes.observe(p)
	}
	fields := []zap.Field{zap.String("tenant", p.Tenant), zap.String("repo", p.Repo), zap.Int("commands", len(p.Commands)),
		zap.Bool("moved", moved), zap.String("unpack", p.unpack)}
	if !p.Who.IsZero() {
		fields = append(fields, zap.String("principal", p.Who.Principal.ID), zap.String("credential", p.Who.Credential))
	}
	if len(p.refused) > 0 {
		for name, why := range p.refused {
			fields = append(fields, zap.String("refused."+name.Short(), why))
			break
		}
	}
	c.logger().Info("git push", fields...)
}

var pktline4Flush = []byte("0000")

func (c *Controller) resultHeaders(w http.ResponseWriter) {
	noCache(w.Header())
	w.Header().Set("Content-Type", "application/x-"+receivePackService+"-result")
}

// checkCommand is what the head itself refuses before any policy: a ref
// that is not a branch, a malformed name, `refs/txco/*` (the application's
// own refs, moved by its ops alone), and a deletion of the default branch.
// Everything else is the tenant stack's to judge [G1].
func checkCommand(repo Repo, cmd *packp.Command) string {
	if cmd.Action() == packp.Invalid {
		return "invalid command"
	}
	name := cmd.Name.String()
	if strings.HasPrefix(name, "refs/txco/") {
		return "refs/txco/* is read-only: the application moves it"
	}
	if !strings.HasPrefix(name, "refs/heads/") {
		return "only branches (refs/heads/*) may be pushed here"
	}
	if len(name) > 255 {
		return "ref name too long"
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x21 || name[i] >= 0x7f {
			return "ref name must be printable ASCII"
		}
	}
	if err := cmd.Name.Validate(); err != nil {
		return "invalid ref name"
	}
	if cmd.New.IsZero() && cmd.Name == repo.Head() {
		return "the default branch cannot be deleted"
	}
	return ""
}

// unpack reads one pack into the staging and answers "ok" or the one-line
// reason it was refused (git prints it after "remote unpack failed:"). The
// header is read first so an over-count pack is refused before any object
// is inflated; a pack of zero objects (a new branch at an existing commit)
// is valid and has nothing to parse.
func (c *Controller) unpack(r io.Reader, staging Staging) string {
	br := bufio.NewReader(r)
	hdr, err := br.Peek(12)
	if err != nil {
		if len(hdr) == 0 && errors.Is(err, io.EOF) {
			return "ok" // no pack at all: every object must already be here
		}
		if too := tooLarge(err, c.limits.MaxPackBytes); too != "" {
			return too
		}
		return "truncated pack"
	}
	if !bytes.Equal(hdr[:4], []byte("PACK")) {
		return "not a pack"
	}
	version := binary.BigEndian.Uint32(hdr[4:8])
	count := binary.BigEndian.Uint32(hdr[8:12])
	if version != packfile.VersionSupported {
		return fmt.Sprintf("unsupported pack version %d", version)
	}
	if c.limits.MaxObjects > 0 && int64(count) > int64(c.limits.MaxObjects) {
		return fmt.Sprintf("too many objects: %d > %d", count, c.limits.MaxObjects)
	}
	if count == 0 {
		_, _ = io.CopyN(io.Discard, br, 32)
		return "ok"
	}
	parser, err := packfile.NewParserWithStorage(packfile.NewScanner(br), staging)
	if err != nil {
		return "unreadable pack"
	}
	if _, err := parser.Parse(); err != nil {
		switch {
		case tooLarge(err, c.limits.MaxPackBytes) != "":
			return tooLarge(err, c.limits.MaxPackBytes)
		case errors.Is(err, gitcore.ErrObjectTooLarge):
			return fmt.Sprintf("an object is over %d bytes", c.limits.MaxObjectBytes)
		case errors.Is(err, gitcore.ErrTooManyObjects):
			return fmt.Sprintf("too many objects: more than %d", c.limits.MaxObjects)
		}
		c.logger().Debug("git push: pack not parsed", zap.Error(err))
		return "pack could not be parsed"
	}
	return "ok"
}

func tooLarge(err error, max int64) string {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		return fmt.Sprintf("pack too large: over %d bytes", max)
	}
	return ""
}

// inspect lists what a command changes, old tree → new tree, and refuses
// what the head's limits forbid: a tip that is not a commit, a symlink, an
// executable, a submodule, an object over the cap, a blob that is not
// text (when --git-text-only, unless the repository's content policy is
// "stack": then the lane decides from the `text`/`type` facts).
func (c *Controller) inspect(s storer.EncodedObjectStorer, meta gitcore.Meta, cmd *packp.Command) ([]Change, string) {
	newCommit, err := object.GetCommit(s, cmd.New)
	if err != nil {
		return nil, fmt.Sprintf("%s must point at a commit", cmd.Name.Short())
	}
	newTree, err := newCommit.Tree()
	if err != nil {
		return nil, "unreadable tree"
	}
	var oldTree *object.Tree
	if !cmd.Old.IsZero() {
		if oldCommit, err := object.GetCommit(s, cmd.Old); err == nil {
			oldTree, _ = oldCommit.Tree()
		}
	}
	diffs, err := object.DiffTree(oldTree, newTree)
	if err != nil {
		return nil, "could not diff the trees"
	}
	out := make([]Change, 0, len(diffs))
	for _, d := range diffs {
		action, err := d.Action()
		if err != nil {
			return nil, "could not diff the trees"
		}
		entry, op := d.To, "modify"
		switch action {
		case merkletrie.Insert:
			op = "add"
		case merkletrie.Delete:
			op, entry = "delete", d.From
		}
		ch := Change{Path: entry.Name, Hash: entry.TreeEntry.Hash, Mode: entry.TreeEntry.Mode, Op: op}
		if op != "delete" {
			if c.limits.TextOnly {
				switch ch.Mode {
				case filemode.Symlink:
					return nil, fmt.Sprintf("%s is a symlink; only regular text files are accepted", ch.Path)
				case filemode.Executable:
					return nil, fmt.Sprintf("%s is executable; only regular text files are accepted", ch.Path)
				case filemode.Submodule:
					return nil, fmt.Sprintf("%s is a submodule; only regular text files are accepted", ch.Path)
				}
			}
			if size, err := s.EncodedObjectSize(ch.Hash); err == nil {
				ch.Size = size
				if c.limits.MaxObjectBytes > 0 && size > c.limits.MaxObjectBytes {
					return nil, fmt.Sprintf("%s is %d bytes; the cap is %d", ch.Path, size, c.limits.MaxObjectBytes)
				}
			}
			text, typ, err := blobFacts(s, ch.Hash)
			if err != nil {
				return nil, fmt.Sprintf("%s could not be read", ch.Path)
			}
			ch.Text, ch.Type = text, typ
			if c.limits.TextOnly && !text && meta.PolicyContent != "stack" {
				return nil, fmt.Sprintf("%s is not text (UTF-8 without NUL)", ch.Path)
			}
		}
		out = append(out, ch)
	}
	return out, ""
}

// blobFacts reads a blob once and answers whether it is text (UTF-8
// without a NUL byte) and the media type its first 512 bytes sniff as.
func blobFacts(s storer.EncodedObjectStorer, h plumbing.Hash) (text bool, typ string, err error) {
	o, err := s.EncodedObject(plumbing.BlobObject, h)
	if err != nil {
		return false, "", err
	}
	rd, err := o.Reader()
	if err != nil {
		return false, "", err
	}
	defer rd.Close()
	b, err := io.ReadAll(rd)
	if err != nil {
		return false, "", err
	}
	return IsText(b), SniffType(b), nil
}

// IsText is the head's one notion of text: UTF-8 without a NUL byte.
func IsText(b []byte) bool { return utf8.Valid(b) && bytes.IndexByte(b, 0) < 0 }

// SniffType is the media type of a file's first 512 bytes, as
// http.DetectContentType reads them — image/png, application/pdf,
// font/woff2, application/zip (an office template is one), text/plain;
// charset=utf-8, and application/octet-stream when nothing is recognized.
func SniffType(b []byte) string {
	if len(b) > 512 {
		b = b[:512]
	}
	return http.DetectContentType(b)
}

func oldOf(cmds []*packp.Command, name plumbing.ReferenceName) plumbing.Hash {
	for _, cmd := range cmds {
		if cmd.Name == name {
			return cmd.Old
		}
	}
	return plumbing.ZeroHash
}

func shortHash(h plumbing.Hash) string {
	if h.IsZero() {
		return "(none)"
	}
	return h.String()[:12]
}

func dedupe(hs []plumbing.Hash) []plumbing.Hash {
	seen := make(map[plumbing.Hash]bool, len(hs))
	out := make([]plumbing.Hash, 0, len(hs))
	for _, h := range hs {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// oneLine makes a reason fit the report's one pkt-line: no control
// characters, at most 200 characters.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		s = "refused"
	}
	return s
}

// revlistObjects is the reachability walk from one tip, excluding what the
// current tips already reach: the objects a push must have delivered (or
// the repository must already hold), and exactly the set that is kept.
func revlistObjects(s storer.EncodedObjectStorer, tip plumbing.Hash, tips []plumbing.Hash) ([]plumbing.Hash, error) {
	return revlist.Objects(s, []plumbing.Hash{tip}, tips)
}
