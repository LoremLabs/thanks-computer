package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// txco implements git/kubectl-style external subcommand plugins: on
// `txco <name> ...` where <name> is not a built-in, txco runs an executable
// named `txco-<name>`, searched first in the plugins dir ($TXCO_HOME/plugins,
// default ~/.config/txco/plugins) and then on $PATH. This keeps the core CLI a
// single, unmodified install while letting out-of-tree tools (the SaaS overlay's
// `txco-credit`, or any third party's `txco-anything`) add namespaced verbs that
// core knows nothing about — no recompile, no registry.

// pluginPrefix is the executable-name prefix a plugin must carry to be
// discovered as `txco <name>`.
const pluginPrefix = "txco-"

// pluginsDir returns the dedicated plugins directory ($TXCO_HOME/plugins) without
// creating it. ok=false if the home dir can't be resolved.
func pluginsDir() (string, bool) {
	home, ok := auth.HomeDir()
	if !ok {
		return "", false
	}
	return filepath.Join(home, "plugins"), true
}

// findPlugin locates the executable backing `txco <name>`: the plugins dir wins
// over $PATH so a user can drop a plugin in $TXCO_HOME/plugins without editing
// $PATH. Returns ok=false when no such plugin exists.
func findPlugin(name string) (path string, ok bool) {
	bin := pluginPrefix + name
	if dir, ok := pluginsDir(); ok {
		cand := filepath.Join(dir, bin)
		if isExecutableFile(cand) {
			return cand, true
		}
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p, true
	}
	return "", false
}

// execPlugin runs the plugin for name with args, inheriting stdin/stdout/stderr
// and the environment, and propagates its exit code. Returns ok=false when no
// plugin exists (caller falls through to the unknown-subcommand error). Built-in
// subcommands are matched by Dispatch's switch first, so a plugin can never
// shadow a built-in.
func execPlugin(name string, args []string, stdout, stderr io.Writer) (status int, ok bool) {
	path, found := findPlugin(name)
	if !found {
		return 0, false
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), true // plugin ran and chose this exit code
		}
		fmt.Fprintf(stderr, "txco: plugin %s%s failed to start: %v\n", pluginPrefix, name, err)
		return 126, true // 126: command found but not executable (shell convention)
	}
	return 0, true
}

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode()&0o111 != 0
}

// --- `txco plugin list` ----------------------------------------------------

const pluginUsage = `Usage: txco plugin list

  list    List installed txco-* plugins (searched in the plugins dir, then $PATH)

A plugin is an executable named txco-<name>; running ` + "`txco <name> …`" + ` execs it.
`

func runPlugin(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "list" {
		return runPluginList(stdout)
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, pluginUsage)
		return 0
	default:
		fmt.Fprintf(stderr, "txco plugin: unknown verb %q\n\n%s", args[0], pluginUsage)
		return 2
	}
}

type pluginEntry struct{ name, path string }

// discoverPlugins scans the plugins dir then each $PATH directory for executable
// txco-<name> files, deduping by name in precedence order (so the entry that
// would actually run is the one listed). Sorted by name for display.
func discoverPlugins() []pluginEntry {
	var dirs []string
	if dir, ok := pluginsDir(); ok {
		dirs = append(dirs, dir)
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)

	seen := map[string]bool{}
	var out []pluginEntry
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, pluginPrefix) {
				continue
			}
			name := strings.TrimPrefix(n, pluginPrefix)
			if name == "" || seen[name] {
				continue
			}
			full := filepath.Join(d, n)
			if !isExecutableFile(full) {
				continue
			}
			seen[name] = true
			out = append(out, pluginEntry{name: name, path: full})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func runPluginList(stdout io.Writer) int {
	found := discoverPlugins()
	if len(found) == 0 {
		fmt.Fprintln(stdout, "No txco plugins found.")
		fmt.Fprintf(stdout, "Install one as an executable named txco-<name> on your $PATH or in %s/plugins.\n",
			auth.HomePathPretty())
		return 0
	}
	width := 0
	for _, p := range found {
		if len(p.name) > width {
			width = len(p.name)
		}
	}
	for _, p := range found {
		fmt.Fprintf(stdout, "%-*s  %s\n", width, p.name, p.path)
	}
	return 0
}
