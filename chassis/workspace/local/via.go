package local

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// EnvExec names the one setting this provider reads for itself: a command
// prefix every workspace command is handed to, instead of being run here.
//
//	TXCO_WORKSPACE_LOCAL_EXEC="sprite exec -s dev-{name} --"
//	TXCO_WORKSPACE_LOCAL_EXEC="docker exec -i pony-{name}"
//
// It is how a chassis on a laptop drives a real machine without another
// provider: the workspace is still a directory here (its bookkeeping, its
// lease, its name), but a command runs wherever the prefix program puts it.
// The prefix is whitespace-separated words, no quoting; `{tenant}`, `{stack}`
// and `{name}` are replaced in each word (a `/` in a name becomes `-`).
// `txco dev --workspace-local-exec` sets it.
//
// What changes when it is set, and why:
//
//   - The command is run as `<prefix…> /bin/sh -c <script>`. The script is
//     the request's command with its variables written in front of it as
//     `export` lines: a process environment does not cross to another
//     machine, so the variables travel inside the command. That includes a
//     run grant's token, which is therefore in the prefix program's argv on
//     this machine. This is the dev posture, as the provider already is.
//   - The prefix program itself runs with the CHASSIS's environment, not the
//     scrubbed one: it is the operator's own program and needs its login
//     and its configuration (`sprite`, `docker`). Nothing of that
//     environment reaches the command unless the program sends it.
//   - `cwd` beneath the workspace becomes a `cd` relative to wherever the
//     other machine starts a command (its home); an absolute path outside
//     the workspace is used as given, there.
//   - The other machine is not made or removed by Create and Destroy, has no
//     stack trees ($TXCO_STACK_DIR), and cannot be dialled (the connect
//     verb) or held open (sessions, a terminal): the provider says so
//     rather than quietly doing those things on this machine.
//   - Its commands do not reach this chassis's grant socket, so a grant is
//     handed over as a provider elsewhere gets it: the token and the run's
//     name, not the socket.
//   - A timeout kills the prefix program. What it had started on the other
//     machine may run on; that is the program's to end.
const EnvExec = "TXCO_WORKSPACE_LOCAL_EXEC"

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseVia splits the prefix into words. Empty means "run commands here".
func parseVia(prefix string) []string {
	return strings.Fields(prefix)
}

// viaWords is the prefix for one workspace: its placeholders filled in.
func viaWords(via []string, tenant, stack, name string) []string {
	r := strings.NewReplacer(
		"{tenant}", tenant,
		"{stack}", stack,
		"{name}", strings.ReplaceAll(name, "/", "-"),
	)
	out := make([]string, len(via))
	for i, w := range via {
		out[i] = r.Replace(w)
	}
	return out
}

// shQuote is one POSIX-shell word, whatever the text.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// viaArgv builds what to run on THIS machine for a request that is carried
// out on another: the prefix, then a shell whose script sets the request's
// variables, changes directory if asked, and runs the command.
func (c *computer) viaArgv(req workspace.ExecRequest) ([]string, error) {
	switch {
	case req.Command != "" && len(req.Args) > 0:
		return nil, &workspace.Error{Code: "bad_request", Message: "set command or args, not both"}
	case req.Command == "" && len(req.Args) == 0:
		return nil, &workspace.Error{Code: "bad_request", Message: "no command or args"}
	case len(req.Args) > 0 && req.Args[0] == "":
		return nil, &workspace.Error{Code: "bad_request", Message: "args[0] is empty"}
	}
	var b strings.Builder
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		if !envKeyRe.MatchString(k) {
			return nil, &workspace.Error{Code: "bad_request", Message: fmt.Sprintf("env name %q cannot be exported", k)}
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export " + k + "=" + shQuote(req.Env[k]) + "\n")
	}
	if req.Cwd != "" {
		dir := filepath.Clean(req.Cwd)
		if filepath.IsAbs(dir) && within(c.dir, dir) {
			// The processor resolved $HOME to this machine's directory; on
			// the other machine the same place is relative to its home.
			rel, err := filepath.Rel(c.dir, dir)
			if err != nil {
				return nil, &workspace.Error{Code: "bad_request", Message: "cwd: " + err.Error()}
			}
			dir = rel
		} else if !filepath.IsAbs(dir) && !within(c.dir, filepath.Join(c.dir, dir)) {
			return nil, &workspace.Error{Code: "bad_request", Message: "cwd escapes the workspace"}
		}
		if dir != "." {
			b.WriteString("cd -- " + shQuote(filepath.ToSlash(dir)) + " || exit 127\n")
		}
	}
	argv := append([]string{}, c.via...)
	if req.Command != "" {
		b.WriteString(req.Command)
		return append(argv, "/bin/sh", "-c", b.String()), nil
	}
	b.WriteString(`exec "$@"`)
	argv = append(argv, "/bin/sh", "-c", b.String(), "sh")
	return append(argv, req.Args...), nil
}

// viaEnv is the prefix program's own environment: the chassis's.
func viaEnv() []string { return os.Environ() }

// errVia is what a verb answers when it cannot be carried out through a
// prefix: doing it on this machine instead would be the wrong machine.
func errVia(what string) error {
	return &workspace.Error{Code: "unsupported", Message: what + " is not available through " + EnvExec + ": the workspace's commands run on another machine"}
}
