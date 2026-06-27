package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// runEdit: `txco edit <stack> <path> [--version N]`
//
// Sugar verb that wraps the PATCH single-file endpoint: GET the draft
// version's current content for <path>, capture its content_hash,
// spawn $EDITOR on a temp copy, and PATCH the saved result back with
// the captured hash as base_hash. Used for one-off rule fixes
// without round-tripping the whole bundle.
//
// Default version: the most recent draft on <stack>; if none exists,
// `CreateDraft(from="active")` runs first. With --version N, that
// exact draft is targeted (server returns 409 if it's already been
// activated).
//
// On a 409 from PATCH, the temp file is preserved so the user's
// edits aren't lost — re-run after pulling the latest state.
func runEdit(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("edit", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	tf := bindTargetFlags(fs)
	versionFlag := fs.Int64("version", 0, "draft version_number to edit (default: most recent draft; auto-creates one from active if none exists)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco edit [flags] <stack> <path>

Open $EDITOR on a single file from a draft of <stack> and PATCH the
result back on save. <path> is the stack-relative file path,
e.g. "100/main.txcl" or "200/mock-response.json".

Without --version: targets the most recent draft, creating one from
the active version if there's no draft.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, "edit: missing <stack> <path>")
		return 2
	}
	stack := fs.Arg(0)
	filePath := fs.Arg(1)

	dir, err := workspaceDir("")
	if err != nil {
		fmt.Fprintf(stderr, "edit: resolve dir: %v\n", err)
		return 1
	}
	if err := confirmMutationTF(dir, tf, false, stderr); err != nil {
		auth.PrintCLIErrorf(stderr, "edit: %v", err)
		return 1
	}
	clientTarget := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	clientTarget.Tenant = resolveTenant(tf.Tenant, effectiveProfile(tf.Target, tf.Profile))
	c := client.New(clientTarget)
	ctx := context.Background()

	versionNumber, err := resolveDraftVersion(ctx, c, stack, *versionFlag, stderr)
	if err != nil {
		auth.PrintCLIErrorf(stderr, "edit: %v", err)
		return 1
	}

	initialContent, baseHash, err := loadDraftFile(ctx, c, stack, versionNumber, filePath)
	if err != nil {
		auth.PrintCLIErrorf(stderr, "edit: %v", err)
		return 1
	}

	tmpPath, err := writeEditBuffer(stack, filePath, initialContent)
	if err != nil {
		auth.PrintCLIErrorf(stderr, "edit: %v", err)
		return 1
	}

	if err := spawnEditor(tmpPath, stdout, stderr); err != nil {
		auth.PrintCLIErrorf(stderr, "edit: %v", err)
		return 1
	}

	newBytes, err := os.ReadFile(tmpPath)
	if err != nil {
		auth.PrintCLIErrorf(stderr, "edit: read temp: %v", err)
		return 1
	}
	newContent := string(newBytes)
	if newContent == initialContent {
		fmt.Fprintln(stdout, "no changes")
		_ = os.Remove(tmpPath)
		return 0
	}

	if strings.HasSuffix(filePath, ".txcl") {
		if _, err := txcl.Resonator(newContent); err != nil {
			auth.PrintCLIErrorf(stderr, "edit: txcl parse error: %v", err)
			fmt.Fprintf(stderr, "edit: your edits are preserved at %s; re-run to retry\n", tmpPath)
			return 1
		}
	}

	res, err := c.PatchDraftFile(ctx, stack, versionNumber, filePath, newContent, baseHash)
	if err != nil {
		var he *client.HTTPError
		if errors.As(err, &he) && he.StatusCode == http.StatusConflict {
			auth.PrintCLIErrorf(stderr, "edit: conflict (%s) — the draft was modified while you were editing", he.Code)
			fmt.Fprintf(stderr, "edit: your edits are preserved at %s\n", tmpPath)
			fmt.Fprintf(stderr, "edit: re-run `txco edit %s %s` to start over from the latest draft\n", stack, filePath)
			return 1
		}
		auth.PrintCLIErrorf(stderr, "edit: %v", err)
		return 1
	}
	fmt.Fprintf(stdout, "patched %s v%d %s (manifest %s)\n",
		stack, versionNumber, res.Path, shortHash(res.ManifestHash))
	_ = os.Remove(tmpPath)
	return 0
}

// resolveDraftVersion picks the version_number to edit. With explicit
// versionFlag != 0, returns it as-is (server enforces draft status).
// With 0, returns the most recent draft; if there's no draft, creates
// one from active and returns its number, logging the auto-create to
// stderr so the user knows it happened.
func resolveDraftVersion(ctx context.Context, c *client.Client, stack string, versionFlag int64, stderr io.Writer) (int64, error) {
	if versionFlag != 0 {
		return versionFlag, nil
	}
	versions, err := c.ListVersions(ctx, stack)
	if err != nil {
		return 0, fmt.Errorf("list versions: %w", err)
	}
	for _, v := range versions {
		if v.Status == "draft" {
			return v.VersionNumber, nil
		}
	}
	n, err := c.CreateDraft(ctx, stack, "active")
	if err != nil {
		return 0, fmt.Errorf("create draft: %w", err)
	}
	fmt.Fprintf(stderr, "edit: created draft %s v%d from active\n", stack, n)
	return n, nil
}

// loadDraftFile fetches the version's files and returns the named
// file's content + content_hash. If the path doesn't exist, returns
// empty content and empty hash — the caller is creating a new file
// and the empty hash signals that to PATCH.
func loadDraftFile(ctx context.Context, c *client.Client, stack string, versionNumber int64, filePath string) (string, string, error) {
	vd, err := c.GetVersion(ctx, stack, versionNumber, true)
	if err != nil {
		return "", "", fmt.Errorf("get version: %w", err)
	}
	for _, f := range vd.Files {
		if f.Path == filePath {
			return f.Content, f.ContentHash, nil
		}
	}
	return "", "", nil
}

// writeEditBuffer drops the initial content into a uniquely-named
// temp file under $TMPDIR. Slashes in the stack-relative path become
// dashes so the temp file name stays a single segment.
func writeEditBuffer(stack, filePath, content string) (string, error) {
	safeStack := strings.ReplaceAll(stack, "/", "-")
	safePath := strings.ReplaceAll(filePath, "/", "-")
	tmp := filepath.Join(os.TempDir(), "txco-edit-"+safeStack+"-"+safePath)
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write temp file: %w", err)
	}
	return tmp, nil
}

// spawnEditor runs $EDITOR (falling back to vi) on the temp file
// with stdin/stdout/stderr wired through so a terminal editor like
// vim has a real TTY.
func spawnEditor(tmpPath string, stdout, stderr io.Writer) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = "vi"
	}
	cmd := exec.Command(editor, tmpPath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("$EDITOR (%s) exited with error: %w", editor, err)
	}
	return nil
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
