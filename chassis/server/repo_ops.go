package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/event"
	gitcore "github.com/loremlabs/thanks-computer/chassis/git"
	"github.com/loremlabs/thanks-computer/chassis/gitstore"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	gitp "github.com/loremlabs/thanks-computer/chassis/server/personality/git"
)

// repo_ops.go holds the txco://repo/* ops: a stack's door to the
// repositories the `git` personality serves. A stack makes a repository
// (`repo/create`), reads the files at a commit (`repo/read`), writes one
// commit for one logical change (`repo/commit`, guarded by `expected` so a
// lost update is refused), lists history (`repo/log`), diffs two commits
// (`repo/diff`), lists refs (`repo/refs`), moves one by compare-and-swap
// (`repo/ref/update`) — the application's own refs (`refs/txco/*`)
// included, which no client may push — and changes a repository's
// policies (`repo/policy`). Nobody but a developer needs git.
//
// Scoping is trusted: the tenant from processor.TenantScope(ctx); a
// repository is addressed by NAME and must belong to the tenant; any stack
// of the tenant may read and write; `repo/remove` is the creating stack's.
// Errors surface under `<into>.error.{code,message}` with a nil Go error
// (the drive shape) so authors branch with `WHEN ._repo.error ...`.

// repoDeps is what the handlers need from the boot wiring.
type repoDeps struct {
	repos *gitcore.StoreRepos // nil ⇒ txco_repo_disabled
	now   func() time.Time
}

var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

const (
	// repoReadDefaultMax caps the content one repo/read returns into the
	// envelope (it is buffered JSON).
	repoReadDefaultMax = 1 << 20
	repoReadCeiling    = 8 << 20
	repoLogMax         = 100
	repoDiffDefaultMax = 1 << 20
)

func repoErr(into, code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, into+".error.code", code)
	raw, _ = sjson.Set(raw, into+".error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

func repoPrelude(ctx context.Context, d repoDeps) (tenant, stack string, meta []byte, into string, ep event.Payload, ok bool) {
	meta = []byte(operation.MetaFromContext(ctx))
	into = intoPath(meta, "_repo")
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", "", nil, into, repoErr(into, "txco_repo_no_tenant", "no tenant in request scope"), false
	}
	if d.repos == nil {
		return "", "", nil, into, repoErr(into, "txco_repo_disabled", "no git store on this node (git personality off and --git-store=sqlite, or the shared store failed to open at boot)"), false
	}
	return tenant, authn.BaseStack(processor.StackScope(ctx)), meta, into, event.Payload{}, true
}

// repoOpen resolves WITH `name` to the tenant's repository.
func repoOpen(ctx context.Context, d repoDeps, tenant string, meta []byte, into string) (gitcore.Repo, event.Payload, bool) {
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if !repoNameRE.MatchString(name) {
		return nil, repoErr(into, "txco_repo_invalid_arg", "`name` must be [A-Za-z0-9_][A-Za-z0-9._-]{0,63}"), false
	}
	repo, found, err := d.repos.Open(ctx, tenant, name)
	if err != nil {
		return nil, repoErr(into, "txco_repo_store", err.Error()), false
	}
	if !found {
		return nil, repoErr(into, "txco_repo_not_found", "no repository named "+name), false
	}
	return repo, event.Payload{}, true
}

// repoResolve turns WITH `ref` / `commit` (or a default) into a commit id:
// a 40-hex id as is; `main` and `active` as refs/heads/main and
// refs/txco/active; any `refs/…` name by lookup. zero + false when the ref
// does not exist.
func repoResolve(ctx context.Context, repo gitcore.Repo, rev string) (plumbing.Hash, bool, error) {
	rev = strings.TrimSpace(rev)
	if rev == "" {
		rev = repo.Head().String()
	}
	if plumbing.IsHash(rev) {
		h := plumbing.NewHash(rev)
		if err := repo.HasEncodedObject(h); err != nil {
			return plumbing.ZeroHash, false, nil
		}
		return h, true, nil
	}
	name := repoRefName(rev)
	refs, err := repo.Refs(ctx)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	for _, r := range refs {
		if r.Name == name {
			return r.Hash, true, nil
		}
	}
	return plumbing.ZeroHash, false, nil
}

// repoRefName expands the short names: `main` → refs/heads/main,
// `active` → refs/txco/active; a full `refs/…` passes through.
func repoRefName(s string) plumbing.ReferenceName {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "refs/"):
		return plumbing.ReferenceName(s)
	case s == "active":
		return plumbing.ReferenceName("refs/txco/active")
	case s == "":
		return plumbing.ReferenceName("refs/heads/main")
	}
	return plumbing.NewBranchReferenceName(s)
}

func repoActor(stack string) gitcore.Actor {
	return gitcore.Actor{Principal: "stack:" + stack, Source: "op", Stack: stack}
}

// ── repo/create ──────────────────────────────────────────────────────────

// repoPolicies reads WITH `policy.{push, content}` and validates the pair:
// push is "stack" (default: a push is accepted only when the tenant's _git
// stack says ok) or "open" (no lane); content is "text" (default: the head
// refuses a blob that is not text) or "stack" (the _git stack decides from
// the `text`/`type` facts on each changed file — docs/0061 §8), which
// needs push "stack", since without a lane nobody would judge. With
// partial=true an absent field stays "" (repo/policy leaves it alone), and
// `cur` supplies the other half for the pair's check.
func repoPolicies(meta []byte, partial bool, cur gitcore.Meta) (push, content, bad string) {
	push = strings.TrimSpace(gjson.GetBytes(meta, "policy.push").String())
	content = strings.TrimSpace(gjson.GetBytes(meta, "policy.content").String())
	switch push {
	case "stack", "open":
	case "":
		if !partial {
			push = "stack"
		}
	default:
		return "", "", "`policy.push` must be stack or open"
	}
	switch content {
	case "text", "stack":
	case "":
		if !partial {
			content = "text"
		}
	default:
		return "", "", "`policy.content` must be text or stack"
	}
	effPush, effContent := push, content
	if effPush == "" {
		effPush = cur.PolicyPush
	}
	if effContent == "" {
		effContent = cur.PolicyContent
	}
	if effContent == "stack" && effPush != "stack" {
		return "", "", "`policy.content` stack needs `policy.push` stack: a _git stack must judge what the head no longer refuses"
	}
	return push, content, ""
}

// repoCreate makes a repository: `name`; `principal` (the identity whose
// credentials the application issues for it); `policy.push` and
// `policy.content` (repoPolicies); `quotas.{max_object_bytes,
// max_pack_bytes, max_repo_bytes, max_objects}` to override the head's
// caps. Idempotent: an existing repository of that name is returned with
// created=false.
func repoCreate(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, stack, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if !repoNameRE.MatchString(name) {
		return repoErr(into, "txco_repo_invalid_arg", "`name` must be [A-Za-z0-9_][A-Za-z0-9._-]{0,63}"), nil
	}
	policy, content, bad := repoPolicies(meta, false, gitcore.Meta{})
	if bad != "" {
		return repoErr(into, "txco_repo_invalid_arg", bad), nil
	}
	row := gitstore.Repo{
		Tenant: tenant, Name: name,
		Principal:     strings.TrimSpace(gjson.GetBytes(meta, "principal").String()),
		CreatedBy:     stack,
		PolicyPush:    policy,
		PolicyContent: content,
		Quotas: gitstore.Quotas{
			MaxObjectBytes: gjson.GetBytes(meta, "quotas.max_object_bytes").Int(),
			MaxPackBytes:   gjson.GetBytes(meta, "quotas.max_pack_bytes").Int(),
			MaxRepoBytes:   gjson.GetBytes(meta, "quotas.max_repo_bytes").Int(),
			MaxObjects:     gjson.GetBytes(meta, "quotas.max_objects").Int(),
		},
	}
	repo, created, err := d.repos.Create(ctx, row)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	m := repo.Meta()
	out := jsonx.NewObject()
	out.Set(into+".id", m.ID)
	out.Set(into+".name", m.Name)
	out.Set(into+".created", created)
	out.Set(into+".principal", m.Principal)
	out.Set(into+".created_by", m.CreatedBy)
	out.Set(into+".policy.push", m.PolicyPush)
	out.Set(into+".policy.content", m.PolicyContent)
	out.Set(into+".head", repo.Head().String())
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// ── repo/policy ──────────────────────────────────────────────────────────

// repoPolicy changes an existing repository's policies: `name`,
// `policy.{push, content}` — a field left out stays as it is; the pair is
// validated as at create (repoPolicies). Answers the policies now in
// force and `changed` (false when nothing differed).
func repoPolicy(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, _, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	repo, ep, ok := repoOpen(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	cur := repo.Meta()
	push, content, bad := repoPolicies(meta, true, cur)
	if bad != "" {
		return repoErr(into, "txco_repo_invalid_arg", bad), nil
	}
	if push == cur.PolicyPush {
		push = ""
	}
	if content == cur.PolicyContent {
		content = ""
	}
	if push != "" || content != "" {
		if err := d.repos.Store().SetRepoPolicy(ctx, cur.ID, push, content); err != nil {
			return repoErr(into, "txco_repo_store", err.Error()), nil
		}
	}
	now := cur
	if push != "" {
		now.PolicyPush = push
	}
	if content != "" {
		now.PolicyContent = content
	}
	out := jsonx.NewObject()
	out.Set(into+".name", cur.Name)
	out.Set(into+".policy.push", now.PolicyPush)
	out.Set(into+".policy.content", now.PolicyContent)
	out.Set(into+".changed", push != "" || content != "")
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// ── repo/remove ──────────────────────────────────────────────────────────

// repoRemove deletes a repository's index rows (its objects' content stays
// in the content-addressed store). The creating stack's to do;
// `not_found` is answered as removed=false, not an error, so a retire may
// run twice.
func repoRemove(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, stack, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	name := strings.TrimSpace(gjson.GetBytes(meta, "name").String())
	if !repoNameRE.MatchString(name) {
		return repoErr(into, "txco_repo_invalid_arg", "`name` must be [A-Za-z0-9_][A-Za-z0-9._-]{0,63}"), nil
	}
	row, found, err := d.repos.Store().GetRepo(ctx, tenant, name)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".name", name)
	if !found {
		out.Set(into+".removed", false)
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	if row.CreatedBy != "" && row.CreatedBy != stack {
		return repoErr(into, "txco_repo_forbidden", "repository "+name+" was made by stack "+row.CreatedBy+"; only it may remove it"), nil
	}
	if err := d.repos.Store().RemoveRepo(ctx, row.ID); err != nil && !errors.Is(err, gitstore.ErrNotFound) {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	out.Set(into+".removed", true)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// ── repo/refs ────────────────────────────────────────────────────────────

// repoRefs lists a repository's refs: `refs` as [{name, oid}], plus the
// two a rule reads by name — `main` (the head branch's id) and `active`
// (refs/txco/active) — "" when absent.
func repoRefs(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, _, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	repo, ep, ok := repoOpen(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	refs, err := repo.Refs(ctx)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".name", repo.Meta().Name)
	out.Set(into+".head", repo.Head().String())
	main, active := "", ""
	list := make([]map[string]string, 0, len(refs))
	for _, r := range refs {
		list = append(list, map[string]string{"name": r.Name.String(), "oid": r.Hash.String()})
		switch r.Name {
		case repo.Head():
			main = r.Hash.String()
		case "refs/txco/active":
			active = r.Hash.String()
		}
	}
	out.Set(into+".main", main)
	out.Set(into+".active", active)
	out.Set(into+".refs", list)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// ── repo/read ────────────────────────────────────────────────────────────

// repoRead returns the files at a commit: WITH `ref` (default the head
// branch; `active` for refs/txco/active) or `commit`; `prefix` to take one
// directory; `paths` for exact files; `max_bytes` (default 1 MiB, ceiling
// 8 MiB) caps the content returned — files past it are listed without
// content and `over_limit` is set. `found` is false when the ref does not
// exist (an empty repository). A file that is not text (UTF-8 without a
// NUL) is `binary: true` with its sniffed `type`, and `binary` says what
// else happens to it (docs/0061 §8): "skip" (default) lists it with no
// content and counts its size toward `max_bytes`, as before; "list"
// lists it and counts nothing, so a configuration's read is not pushed
// over its limit by an image beside it; "base64" returns its bytes as
// `content_base64`, counted.
func repoRead(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, _, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	repo, ep, ok := repoOpen(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	rev := gjson.GetBytes(meta, "commit").String()
	if rev == "" {
		rev = gjson.GetBytes(meta, "ref").String()
	}
	h, found, err := repoResolve(ctx, repo, rev)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".name", repo.Meta().Name)
	if !found {
		out.Set(into+".found", false)
		out.Set(into+".files", []any{})
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	commit, err := object.GetCommit(repo, h)
	if err != nil {
		return repoErr(into, "txco_repo_invalid_arg", rev+" is not a commit"), nil
	}
	tree, err := commit.Tree()
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	prefix := strings.Trim(strings.TrimSpace(gjson.GetBytes(meta, "prefix").String()), "/")
	if prefix != "" {
		prefix += "/"
	}
	want := map[string]bool{}
	for _, p := range gjson.GetBytes(meta, "paths").Array() {
		want[strings.Trim(p.String(), "/")] = true
	}
	max := gjson.GetBytes(meta, "max_bytes").Int()
	if max <= 0 {
		max = repoReadDefaultMax
	}
	if max > repoReadCeiling {
		max = repoReadCeiling
	}
	binaryMode := strings.TrimSpace(gjson.GetBytes(meta, "binary").String())
	switch binaryMode {
	case "", "skip":
		binaryMode = "skip"
	case "list", "base64":
	default:
		return repoErr(into, "txco_repo_invalid_arg", "`binary` must be skip, list or base64"), nil
	}
	var used int64
	over := false
	files := make([]map[string]any, 0, 16)
	err = tree.Files().ForEach(func(f *object.File) error {
		if prefix != "" && !strings.HasPrefix(f.Name, prefix) {
			return nil
		}
		if len(want) > 0 && !want[f.Name] {
			return nil
		}
		entry := map[string]any{"path": f.Name, "size": f.Size, "mode": f.Mode.String(), "oid": f.Hash.String()}
		// In "list" mode a binary counts toward nothing, so it must be read
		// (and told apart from text) before the budget is consulted.
		if binaryMode != "list" && used+f.Size > max {
			over = true
			files = append(files, entry)
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return err
		}
		if gitp.IsText([]byte(content)) {
			if used+f.Size > max {
				over = true
				files = append(files, entry)
				return nil
			}
			entry["content"] = content
			used += f.Size
			files = append(files, entry)
			return nil
		}
		entry["binary"] = true
		entry["type"] = gitp.SniffType([]byte(content))
		switch binaryMode {
		case "base64":
			entry["content_base64"] = base64.StdEncoding.EncodeToString([]byte(content))
			used += f.Size
		case "skip":
			used += f.Size
		}
		files = append(files, entry)
		return nil
	})
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	out.Set(into+".found", true)
	out.Set(into+".commit", h.String())
	out.Set(into+".tree", tree.Hash.String())
	out.Set(into+".over_limit", over)
	out.Set(into+".files", files)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// ── repo/commit ──────────────────────────────────────────────────────────

// repoCommit writes one commit for one logical change: WITH `ref` (a
// branch; default the head branch), `message`, `writes` [{path, content}],
// `deletes` [path], `author` {name, email}; `expected` is the ref's
// current id (ZeroOID or "" for a ref that must not exist yet) and the
// write is refused with txco_repo_moved when the ref is elsewhere — the
// lost-update guard; `base` is the parent commit for a ref that does not
// exist yet (a proposal branched from `active`); `tree` is a commit whose
// tree the new commit copies (a rollback). The new tree is the parent's
// with the writes and deletes applied; directories come and go with their
// files. Nothing is written when there is nothing to change.
func repoCommit(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, stack, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	repo, ep, ok := repoOpen(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	ref := repoRefName(gjson.GetBytes(meta, "ref").String())
	if !strings.HasPrefix(ref.String(), "refs/heads/") || ref.Validate() != nil {
		return repoErr(into, "txco_repo_invalid_arg", "`ref` must be a branch (refs/heads/…)"), nil
	}
	message := strings.TrimSpace(gjson.GetBytes(meta, "message").String())
	if message == "" {
		return repoErr(into, "txco_repo_invalid_arg", "`message` is required"), nil
	}
	// Where the ref is now.
	current := plumbing.ZeroHash
	refs, err := repo.Refs(ctx)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	for _, r := range refs {
		if r.Name == ref {
			current = r.Hash
		}
	}
	if exp := gjson.GetBytes(meta, "expected"); exp.Exists() {
		want := plumbing.ZeroHash
		if s := strings.TrimSpace(exp.String()); s != "" {
			if !plumbing.IsHash(s) {
				return repoErr(into, "txco_repo_invalid_arg", "`expected` must be a commit id or empty"), nil
			}
			want = plumbing.NewHash(s)
		}
		if want != current {
			ep := repoErr(into, "txco_repo_moved", fmt.Sprintf("%s is at %s, not %s", ref.Short(), shortOID(current), shortOID(want)))
			raw, _ := sjson.Set(ep.Raw, into+".current", current.String())
			return event.Payload{Raw: raw, Type: event.JSON}, nil
		}
	}
	// The parent and the tree to start from.
	parent := current
	if parent.IsZero() {
		if b := strings.TrimSpace(gjson.GetBytes(meta, "base").String()); b != "" {
			bh, found, err := repoResolve(ctx, repo, b)
			if err != nil {
				return repoErr(into, "txco_repo_store", err.Error()), nil
			}
			if !found {
				return repoErr(into, "txco_repo_invalid_arg", "`base` names no commit"), nil
			}
			parent = bh
		}
	}
	entries := map[string]object.TreeEntry{} // path → entry (files only)
	from := parent
	if t := strings.TrimSpace(gjson.GetBytes(meta, "tree").String()); t != "" {
		th, found, err := repoResolve(ctx, repo, t)
		if err != nil {
			return repoErr(into, "txco_repo_store", err.Error()), nil
		}
		if !found {
			return repoErr(into, "txco_repo_invalid_arg", "`tree` names no commit"), nil
		}
		from = th
	}
	if !from.IsZero() {
		c, err := object.GetCommit(repo, from)
		if err != nil {
			return repoErr(into, "txco_repo_invalid_arg", "the base is not a commit"), nil
		}
		t, err := c.Tree()
		if err != nil {
			return repoErr(into, "txco_repo_store", err.Error()), nil
		}
		if err := t.Files().ForEach(func(f *object.File) error {
			entries[f.Name] = object.TreeEntry{Name: f.Name, Mode: f.Mode, Hash: f.Hash}
			return nil
		}); err != nil {
			return repoErr(into, "txco_repo_store", err.Error()), nil
		}
	}
	// Apply the change.
	changed := 0
	for _, p := range gjson.GetBytes(meta, "deletes").Array() {
		path, ok := cleanRepoPath(p.String())
		if !ok {
			return repoErr(into, "txco_repo_invalid_arg", "bad path in `deletes`: "+p.String()), nil
		}
		if _, had := entries[path]; had {
			delete(entries, path)
			changed++
		}
	}
	limits := d.repos.Limits()
	if lr, ok := repo.(interface{ Limits() gitcore.Limits }); ok {
		limits = lr.Limits()
	}
	for _, w := range gjson.GetBytes(meta, "writes").Array() {
		path, ok := cleanRepoPath(w.Get("path").String())
		if !ok {
			return repoErr(into, "txco_repo_invalid_arg", "bad path in `writes`: "+w.Get("path").String()), nil
		}
		content := w.Get("content").String()
		// A write's `encoding: "base64"` carries bytes (an importer's image or
		// template, docs/0061 §8); the content policy below decides whether
		// the repository takes them.
		switch enc := strings.TrimSpace(w.Get("encoding").String()); enc {
		case "":
		case "base64":
			raw, err := base64.StdEncoding.DecodeString(content)
			if err != nil {
				return repoErr(into, "txco_repo_invalid_arg", path+": `content` is not base64"), nil
			}
			content = string(raw)
		default:
			return repoErr(into, "txco_repo_invalid_arg", path+": `encoding` must be base64 or left out"), nil
		}
		if limits.MaxObjectBytes > 0 && int64(len(content)) > limits.MaxObjectBytes {
			return repoErr(into, "txco_repo_too_large", fmt.Sprintf("%s is %d bytes; the cap is %d", path, len(content), limits.MaxObjectBytes)), nil
		}
		// Text only, unless the repository's content policy hands the
		// decision to the stack — which, for an op's commit, is the caller:
		// it applies the same check a push gets before it writes.
		if limits.TextOnly && !gitp.IsText([]byte(content)) && repo.Meta().PolicyContent != "stack" {
			return repoErr(into, "txco_repo_invalid_arg", path+" is not text (UTF-8 without NUL)"), nil
		}
		blob := repo.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		blob.SetSize(int64(len(content)))
		wr, err := blob.Writer()
		if err != nil {
			return repoErr(into, "txco_repo_store", err.Error()), nil
		}
		_, _ = wr.Write([]byte(content))
		_ = wr.Close()
		h, err := repo.SetEncodedObject(blob)
		if err != nil {
			return repoErr(into, "txco_repo_store", err.Error()), nil
		}
		if old, had := entries[path]; !had || old.Hash != h {
			changed++
		}
		entries[path] = object.TreeEntry{Name: path, Mode: filemode.Regular, Hash: h}
	}
	if changed == 0 && gjson.GetBytes(meta, "tree").String() == "" {
		out := jsonx.NewObject()
		out.Set(into+".name", repo.Meta().Name)
		out.Set(into+".ref", ref.String())
		out.Set(into+".commit", current.String())
		out.Set(into+".old", current.String())
		out.Set(into+".unchanged", true)
		return event.Payload{Raw: out.String(), Type: event.JSON}, nil
	}
	treeHash, err := writeTree(repo, entries)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	now := d.now()
	sig := object.Signature{
		Name:  strings.TrimSpace(gjson.GetBytes(meta, "author.name").String()),
		Email: strings.TrimSpace(gjson.GetBytes(meta, "author.email").String()),
		When:  now,
	}
	if sig.Name == "" {
		sig.Name = stack
	}
	if sig.Email == "" {
		sig.Email = stack + "@" + tenant + ".txco"
	}
	commit := &object.Commit{Author: sig, Committer: sig, Message: message + "\n", TreeHash: treeHash}
	if !parent.IsZero() {
		commit.ParentHashes = []plumbing.Hash{parent}
	}
	co := repo.NewEncodedObject()
	if err := commit.Encode(co); err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	ch, err := repo.SetEncodedObject(co)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	if err := repo.UpdateRefs(ctx, []gitcore.RefUpdate{{Name: ref, Old: current, New: ch}}, repoActor(stack)); err != nil {
		var moved *gitcore.ErrRefMoved
		if errors.As(err, &moved) {
			ep := repoErr(into, "txco_repo_moved", fmt.Sprintf("%s moved to %s while the commit was being written", ref.Short(), shortOID(moved.Current)))
			raw, _ := sjson.Set(ep.Raw, into+".current", moved.Current.String())
			return event.Payload{Raw: raw, Type: event.JSON}, nil
		}
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".name", repo.Meta().Name)
	out.Set(into+".ref", ref.String())
	out.Set(into+".commit", ch.String())
	out.Set(into+".tree", treeHash.String())
	out.Set(into+".old", current.String())
	out.Set(into+".changed", changed)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// cleanRepoPath normalizes a file path inside the tree: relative, no
// empty or dot segments, no backslash or control characters.
func cleanRepoPath(p string) (string, bool) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" || len(p) > 1024 {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || seg == ".git" {
			return "", false
		}
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return "", false
		}
	}
	return p, true
}

// writeTree builds the tree objects for a flat path → entry map, bottom
// up, and returns the root tree's id. Entries are ordered as git orders
// them: by name bytes, a directory compared as if its name ended in "/".
func writeTree(repo gitcore.Repo, files map[string]object.TreeEntry) (plumbing.Hash, error) {
	type dir struct {
		files map[string]object.TreeEntry // name → blob entry
		dirs  map[string]*dir
	}
	root := &dir{files: map[string]object.TreeEntry{}, dirs: map[string]*dir{}}
	for path, e := range files {
		segs := strings.Split(path, "/")
		d := root
		for _, s := range segs[:len(segs)-1] {
			child, ok := d.dirs[s]
			if !ok {
				child = &dir{files: map[string]object.TreeEntry{}, dirs: map[string]*dir{}}
				d.dirs[s] = child
			}
			d = child
		}
		name := segs[len(segs)-1]
		d.files[name] = object.TreeEntry{Name: name, Mode: e.Mode, Hash: e.Hash}
	}
	var write func(d *dir) (plumbing.Hash, error)
	write = func(d *dir) (plumbing.Hash, error) {
		entries := make([]object.TreeEntry, 0, len(d.files)+len(d.dirs))
		for _, e := range d.files {
			entries = append(entries, e)
		}
		for name, child := range d.dirs {
			h, err := write(child)
			if err != nil {
				return plumbing.ZeroHash, err
			}
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
		}
		sort.Slice(entries, func(i, j int) bool {
			return treeSortKey(entries[i]) < treeSortKey(entries[j])
		})
		t := &object.Tree{Entries: entries}
		o := repo.NewEncodedObject()
		if err := t.Encode(o); err != nil {
			return plumbing.ZeroHash, err
		}
		return repo.SetEncodedObject(o)
	}
	return write(root)
}

func treeSortKey(e object.TreeEntry) string {
	if e.Mode == filemode.Dir {
		return e.Name + "/"
	}
	return e.Name
}

func shortOID(h plumbing.Hash) string {
	if h.IsZero() {
		return "(none)"
	}
	return h.String()[:12]
}

// ── repo/log ─────────────────────────────────────────────────────────────

// repoLog lists history from WITH `ref` (default the head branch) or
// `commit`, first parent only, newest first, `limit` ≤ 100.
func repoLog(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, _, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	repo, ep, ok := repoOpen(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	rev := gjson.GetBytes(meta, "commit").String()
	if rev == "" {
		rev = gjson.GetBytes(meta, "ref").String()
	}
	h, found, err := repoResolve(ctx, repo, rev)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	limit := int(gjson.GetBytes(meta, "limit").Int())
	if limit <= 0 || limit > repoLogMax {
		limit = 20
	}
	out := jsonx.NewObject()
	out.Set(into+".name", repo.Meta().Name)
	commits := make([]map[string]any, 0, limit)
	for !h.IsZero() && found && len(commits) < limit {
		c, err := object.GetCommit(repo, h)
		if err != nil {
			break
		}
		parents := make([]string, 0, len(c.ParentHashes))
		for _, p := range c.ParentHashes {
			parents = append(parents, p.String())
		}
		commits = append(commits, map[string]any{
			"commit": c.Hash.String(), "parents": parents, "tree": c.TreeHash.String(),
			"author": map[string]string{"name": c.Author.Name, "email": c.Author.Email},
			"when":   c.Author.When.UTC().Format(time.RFC3339), "message": strings.TrimRight(c.Message, "\n"),
		})
		if len(c.ParentHashes) == 0 {
			break
		}
		h = c.ParentHashes[0]
	}
	out.Set(into+".found", found)
	out.Set(into+".commits", commits)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// ── repo/diff ────────────────────────────────────────────────────────────

// repoDiff lists what changed from WITH `base` to `head` (commit ids or
// ref names; an empty `base` is the empty tree): [{path, op, from, to,
// patch}], the unified patch per text file under `max_bytes` (default
// 1 MiB) in all, "" past it or for a file that is not text. `paths`
// limits the files. (`base`/`head`, not `from`/`to`: `to` is an envelope
// target's name in this chassis and is guarded as one.)
func repoDiff(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, _, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	repo, ep, ok := repoOpen(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	var fromTree, toTree *object.Tree
	if f := strings.TrimSpace(gjson.GetBytes(meta, "base").String()); f != "" {
		fh, found, err := repoResolve(ctx, repo, f)
		if err != nil {
			return repoErr(into, "txco_repo_store", err.Error()), nil
		}
		if !found {
			return repoErr(into, "txco_repo_invalid_arg", "`base` names no commit"), nil
		}
		c, err := object.GetCommit(repo, fh)
		if err != nil {
			return repoErr(into, "txco_repo_invalid_arg", "`base` is not a commit"), nil
		}
		if fromTree, err = c.Tree(); err != nil {
			return repoErr(into, "txco_repo_store", err.Error()), nil
		}
	}
	th, found, err := repoResolve(ctx, repo, gjson.GetBytes(meta, "head").String())
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	if !found {
		return repoErr(into, "txco_repo_invalid_arg", "`head` names no commit"), nil
	}
	tc, err := object.GetCommit(repo, th)
	if err != nil {
		return repoErr(into, "txco_repo_invalid_arg", "`head` is not a commit"), nil
	}
	if toTree, err = tc.Tree(); err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	want := map[string]bool{}
	for _, p := range gjson.GetBytes(meta, "paths").Array() {
		want[strings.Trim(p.String(), "/")] = true
	}
	max := gjson.GetBytes(meta, "max_bytes").Int()
	if max <= 0 {
		max = repoDiffDefaultMax
	}
	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	var used int64
	list := make([]map[string]any, 0, len(changes))
	for _, ch := range changes {
		action, err := ch.Action()
		if err != nil {
			continue
		}
		path, op := ch.To.Name, "modify"
		switch action {
		case merkletrie.Insert:
			op = "add"
		case merkletrie.Delete:
			op, path = "delete", ch.From.Name
		}
		if len(want) > 0 && !want[path] {
			continue
		}
		entry := map[string]any{"path": path, "op": op, "from": ch.From.TreeEntry.Hash.String(), "to": ch.To.TreeEntry.Hash.String(), "patch": ""}
		if used < max {
			if p, err := ch.Patch(); err == nil {
				s := p.String()
				if utf8.ValidString(s) && !strings.ContainsRune(s, 0) && used+int64(len(s)) <= max {
					entry["patch"] = s
					used += int64(len(s))
				}
			}
		}
		list = append(list, entry)
	}
	out := jsonx.NewObject()
	out.Set(into+".name", repo.Meta().Name)
	out.Set(into+".head", th.String())
	if fromTree != nil {
		out.Set(into+".base_tree", fromTree.Hash.String())
	}
	out.Set(into+".changes", list)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}

// ── repo/ref/update ──────────────────────────────────────────────────────

// repoRefUpdate moves one ref by compare-and-swap: WITH `ref` (any valid
// name, `refs/txco/*` included — the application's own refs, which no
// client may push), `old` (the current id; "" or ZeroOID for a ref that
// must not exist), `new` (the id to set; "" or ZeroOID to delete). Refused
// with txco_repo_moved (and `current`) when the ref is not at `old`; the
// new id must be an object the repository holds.
func repoRefUpdate(ctx context.Context, d repoDeps, in []byte) (event.Payload, error) {
	tenant, stack, meta, into, ep, ok := repoPrelude(ctx, d)
	if !ok {
		return ep, nil
	}
	repo, ep, ok := repoOpen(ctx, d, tenant, meta, into)
	if !ok {
		return ep, nil
	}
	ref := repoRefName(gjson.GetBytes(meta, "ref").String())
	if !strings.HasPrefix(ref.String(), "refs/") || ref.Validate() != nil {
		return repoErr(into, "txco_repo_invalid_arg", "`ref` must be a valid refs/… name"), nil
	}
	parse := func(key string) (plumbing.Hash, bool) {
		s := strings.TrimSpace(gjson.GetBytes(meta, key).String())
		if s == "" || s == gitstore.ZeroOID {
			return plumbing.ZeroHash, true
		}
		if !plumbing.IsHash(s) {
			return plumbing.ZeroHash, false
		}
		return plumbing.NewHash(s), true
	}
	old, ok := parse("old")
	if !ok {
		return repoErr(into, "txco_repo_invalid_arg", "`old` must be a commit id or empty"), nil
	}
	nw, ok := parse("new")
	if !ok {
		return repoErr(into, "txco_repo_invalid_arg", "`new` must be a commit id or empty"), nil
	}
	if !nw.IsZero() {
		if err := repo.HasEncodedObject(nw); err != nil {
			return repoErr(into, "txco_repo_invalid_arg", "`new` names an object this repository does not hold"), nil
		}
	}
	if ref == repo.Head() && nw.IsZero() {
		return repoErr(into, "txco_repo_invalid_arg", "the default branch cannot be deleted"), nil
	}
	if err := repo.UpdateRefs(ctx, []gitcore.RefUpdate{{Name: ref, Old: old, New: nw}}, repoActor(stack)); err != nil {
		var moved *gitcore.ErrRefMoved
		if errors.As(err, &moved) {
			ep := repoErr(into, "txco_repo_moved", fmt.Sprintf("%s is at %s, not %s", ref.String(), shortOID(moved.Current), shortOID(old)))
			raw, _ := sjson.Set(ep.Raw, into+".current", moved.Current.String())
			return event.Payload{Raw: raw, Type: event.JSON}, nil
		}
		return repoErr(into, "txco_repo_store", err.Error()), nil
	}
	out := jsonx.NewObject()
	out.Set(into+".name", repo.Meta().Name)
	out.Set(into+".ref", ref.String())
	out.Set(into+".old", old.String())
	out.Set(into+".new", nw.String())
	out.Set(into+".ok", true)
	return event.Payload{Raw: out.String(), Type: event.JSON}, nil
}
