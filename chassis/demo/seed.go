package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Seed walks the curriculum and ensures every step's stack exists on
// the chassis at `adminURL`, with the step's ops as the active
// version and `<stack>.<HostSuffix>` bound to it.
//
// Called from `txco demo` after the chassis is ready but before the
// browser opens. By the time the SPA mounts, all stacks are already
// seeded — the Runner's onMount filter sees them via listStacks and
// does nothing, so the user never sees a "step failed to seed" error.
//
// Sequencing: stacks are seeded SERIALLY (one chain at a time). The
// SPA used to run 3 concurrent chains for speed; the chassis's
// SQLite contention story under that load was lossy (~13% of chains
// hit a transient 500). Serial is ~3–4 s wall-clock for ~15 stacks,
// which is fine here — happens once at startup, in the background
// of the spinner.
//
// Best-effort per step: if a step's chain fails (createDraft, etc.),
// Seed logs the failure and continues. The SPA's listStacks filter
// will catch any stack missing an active_version on first paint and
// (re-)seed just those. Worst case: one transient failure → the SPA
// quietly retries it on first load.
func Seed(ctx context.Context, adminURL string) error {
	adminURL = strings.TrimRight(adminURL, "/")
	c := Get()
	client := &http.Client{Timeout: 30 * time.Second}

	// Track failures so the caller can decide whether to surface them
	// — but never bail mid-loop: a single bad step (e.g. a flaky
	// network) shouldn't gate the other 14 working stacks.
	var failed []string
	for _, t := range c.Tracks {
		for _, step := range t.Steps {
			if err := seedStep(ctx, client, adminURL, c.HostSuffix, step); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", step.Stack, err))
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("seed: %d step(s) failed (chassis will accept retries from the SPA):\n  %s",
			len(failed), strings.Join(failed, "\n  "))
	}
	return nil
}

// seedStep runs the ensureStack chain for one step:
//  1. For each op with a JS compute, build the wasm artifact and
//     substitute `op://<name>` → `compute://sha256/<digest>` in its txcl.
//  2. Create a draft (empty — no `from` clone, since we seed every
//     step from scratch at boot).
//  3. PUT the draft's files.
//  4. Validate. A failed validate aborts the chain (so we don't
//     activate broken txcl that would later hang requests).
//  5. Activate the draft as the stack's current version.
//  6. Bind `<stack>.<hostSuffix>` → stack. 409 (already bound) is
//     treated as success — idempotent on chassis restarts that
//     happen to leave the binding intact.
func seedStep(ctx context.Context, client *http.Client, adminURL, hostSuffix string, step Step) error {
	const tenant = "default"
	stack := step.Stack

	// Copy ops because we may rewrite txcl in place for compute
	// substitution; never mutate the package-level Get() result.
	ops := make([]OpFile, len(step.Ops))
	copy(ops, step.Ops)

	for i, op := range ops {
		if op.Js == "" {
			continue
		}
		ref, err := buildCompute(ctx, client, adminURL, op.Js)
		if err != nil {
			return fmt.Errorf("compile %s: %w", op.Name, err)
		}
		// Substitute `op://<name>` → ref. The curriculum's txcl uses
		// `EXEC "op://<name>"` as a placeholder; the SPA does the
		// same substitution at activate time.
		ops[i].Txcl = strings.ReplaceAll(op.Txcl, "op://"+op.Name, ref)
	}

	// 1. createDraft (empty)
	versionN, err := createDraft(ctx, client, adminURL, tenant, stack)
	if err != nil {
		return fmt.Errorf("create draft: %w", err)
	}

	// 2. putDraftFiles — translate ops into the wire's {path, content}
	//    files list. Path convention matches the SPA's Runner exactly
	//    (`<scope>/<name>.txcl`, e.g. `0/hello.txcl`) so re-applies
	//    from the SPA later overwrite — not duplicate — these files.
	files := make([]fileBody, 0, len(ops))
	for _, op := range ops {
		files = append(files, fileBody{
			Path:    fmt.Sprintf("%d/%s.txcl", op.Scope, op.Name),
			Content: op.Txcl,
		})
	}
	if err := putDraftFiles(ctx, client, adminURL, tenant, stack, versionN, files); err != nil {
		return fmt.Errorf("put files: %w", err)
	}

	// 3. validate — abort the chain if the txcl is malformed (matches
	//    the SPA's behavior).
	if err := validateVersion(ctx, client, adminURL, tenant, stack, versionN); err != nil {
		return fmt.Errorf("validate: %w", err)
	}

	// 4. activate
	if err := activateStack(ctx, client, adminURL, tenant, stack, versionN); err != nil {
		return fmt.Errorf("activate: %w", err)
	}

	// 5. bind <stack>.<hostSuffix>
	hostname := stack + "." + hostSuffix
	if err := bindHostname(ctx, client, adminURL, tenant, hostname, stack); err != nil {
		return fmt.Errorf("bind hostname: %w", err)
	}
	return nil
}

// --- admin API thin wrappers (one per endpoint we hit) --------------

// createDraftBody / createDraftResp are the wire shapes for
// POST /v1/tenants/<t>/stacks/<n>/draft. The empty `from` value
// asks the server for a fresh draft (no clone from an existing
// version); the response carries the new draft's version_number.
type createDraftBody struct {
	From string `json:"from"`
}
type createDraftResp struct {
	VersionNumber int `json:"version_number"`
}

func createDraft(ctx context.Context, c *http.Client, adminURL, tenant, stack string) (int, error) {
	body, _ := json.Marshal(createDraftBody{From: ""})
	url := fmt.Sprintf("%s/v1/tenants/%s/stacks/%s/draft", adminURL, tenant, stack)
	var resp createDraftResp
	if err := postJSON(ctx, c, url, body, &resp); err != nil {
		return 0, err
	}
	if resp.VersionNumber == 0 {
		return 0, errors.New("server returned no version_number")
	}
	return resp.VersionNumber, nil
}

type fileBody struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// putDraftFilesBody is the wire shape: the files list is wrapped in
// a `{ "files": [...] }` envelope (matches admin-ui's api.ts).
type putDraftFilesBody struct {
	Files []fileBody `json:"files"`
}

func putDraftFiles(ctx context.Context, c *http.Client, adminURL, tenant, stack string, n int, files []fileBody) error {
	body, _ := json.Marshal(putDraftFilesBody{Files: files})
	url := fmt.Sprintf("%s/v1/tenants/%s/stacks/%s/versions/%d/files", adminURL, tenant, stack, n)
	return putJSON(ctx, c, url, body, nil)
}

// validateVersion: POST .../validate. Server returns a 200 with
// {ok: true} on a clean version or {ok: false, errors: [...]} with a
// non-2xx for parse/ref failures. We just need the boolean.
type validateResp struct {
	OK     bool `json:"ok"`
	Errors []struct {
		Path string `json:"path"`
		Err  string `json:"err"`
	} `json:"errors,omitempty"`
}

func validateVersion(ctx context.Context, c *http.Client, adminURL, tenant, stack string, n int) error {
	url := fmt.Sprintf("%s/v1/tenants/%s/stacks/%s/versions/%d/validate", adminURL, tenant, stack, n)
	var resp validateResp
	if err := postJSON(ctx, c, url, nil, &resp); err != nil {
		return err
	}
	if !resp.OK {
		var msgs []string
		for _, e := range resp.Errors {
			msgs = append(msgs, fmt.Sprintf("%s: %s", e.Path, e.Err))
		}
		return fmt.Errorf("validate failed: %s", strings.Join(msgs, "; "))
	}
	return nil
}

type activateBody struct {
	VersionNumber int `json:"version_number"`
}

func activateStack(ctx context.Context, c *http.Client, adminURL, tenant, stack string, n int) error {
	body, _ := json.Marshal(activateBody{VersionNumber: n})
	url := fmt.Sprintf("%s/v1/tenants/%s/stacks/%s/activate", adminURL, tenant, stack)
	return postJSON(ctx, c, url, body, nil)
}

type bindHostnameBody struct {
	Hostname string `json:"hostname"`
	Stack    string `json:"stack"`
}

// bindHostname: POST /v1/tenants/<t>/hostnames. 409 is treated as
// success (the hostname is already bound to this stack; idempotent).
func bindHostname(ctx context.Context, c *http.Client, adminURL, tenant, hostname, stack string) error {
	body, _ := json.Marshal(bindHostnameBody{Hostname: hostname, Stack: stack})
	url := fmt.Sprintf("%s/v1/tenants/%s/hostnames", adminURL, tenant)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		return nil // already bound — idempotent
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// buildCompute: POST /v1/demo/op/build. Returns the
// `compute://sha256/<digest>` ref the caller substitutes into the
// op's txcl. Always lang=js — the curriculum's nano-ops are
// JavaScript today; if a future track adds TS we'd plumb lang through
// the OpFile type.
type buildBody struct {
	Source string `json:"source"`
	Lang   string `json:"lang"`
}
type buildResp struct {
	Ref string `json:"ref"`
}

func buildCompute(ctx context.Context, c *http.Client, adminURL, source string) (string, error) {
	body, _ := json.Marshal(buildBody{Source: source, Lang: "js"})
	url := adminURL + "/v1/demo/op/build"
	var resp buildResp
	if err := postJSON(ctx, c, url, body, &resp); err != nil {
		return "", err
	}
	if resp.Ref == "" {
		return "", errors.New("server returned empty ref")
	}
	return resp.Ref, nil
}

// --- HTTP helpers ----------------------------------------------------

// postJSON / putJSON: minimal wrapper that POSTs a body, decodes the
// response into `out` if non-nil, and returns a meaningful error on
// any non-2xx status (including the response body trimmed to ~64KB).
func postJSON(ctx context.Context, c *http.Client, url string, body []byte, out any) error {
	return doJSON(ctx, c, http.MethodPost, url, body, out)
}

func putJSON(ctx context.Context, c *http.Client, url string, body []byte, out any) error {
	return doJSON(ctx, c, http.MethodPut, url, body, out)
}

func doJSON(ctx context.Context, c *http.Client, method, url string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
