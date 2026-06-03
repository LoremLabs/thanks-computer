package cli

// Shell-completion support for the txco binary.
//
// `txco completion <bash|zsh|fish>` emits a shell-specific completion
// script to stdout. The user installs once per shell (one-time:
// `source <(txco completion zsh)` in the current shell, or write the
// script to the shell's completion dir for persistence — see
// printCompletionHelp).
//
// **Single source of authority.** The command tree is encoded ONCE in
// `cliCommandTree` below. Three small emitters (emitBash / emitZsh /
// emitFish) walk that tree and produce the appropriate shell syntax.
// Adding a new top-level command means adding one entry to the tree;
// the three scripts pick it up automatically.
//
// **Drift discipline.** A test in completion_test.go AST-walks the
// `Dispatch` switch in cli.go and asserts every non-hidden case arm has
// a corresponding entry here. So if a future PR adds a new top-level
// command and forgets to update this tree, the chassis test suite
// fails loudly.
//
// **Scope.** Command names + flag names only. Flag-value completion
// (tenant slugs, hostnames, stack names) would need a chassis round-
// trip and is not wired here. PowerShell is not generated.

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// node describes one command in the tree. Leaves have nil Children;
// dispatcher nodes carry their subcommands. Aliases is the set of
// alternate names this command also responds to (e.g., "ls" for
// "list"); completion offers both the primary and the aliases so users
// who muscle-memorize an alias still tab-complete from it.
type node struct {
	Name     string
	Desc     string
	Aliases  []string
	Children []node
	// Flags lists the flag names (without leading "--") this command
	// accepts. Optional; when present, the bash/zsh emitters offer
	// these on `--<TAB>`. When absent, the emitters fall back to a
	// chassis-wide common-flag set (commonFlags).
	Flags []string
}

// commonFlags is the fallback flag list when a command's own Flags is
// empty. Sourced from the recurring set across apply/diff/status/dev
// and the auth subcommands. Updating this is a one-line change that
// improves every command's completion at once.
var commonFlags = []string{
	"--profile", "--url", "--tenant", "--stack",
	"--addr", "--target", "--user", "--pass",
	"--dry-run", "--verbose", "--help",
}

// authChildren is the auth dispatcher's subtree. Mirrors the switch in
// chassis/cli/auth/auth.go:17-63.
var authChildren = []node{
	{Name: "bootstrap-local", Desc: "Keygen + enroll + write meta (one-shot dev setup)"},
	{Name: "init", Desc: "Same as enroll"},
	{Name: "enroll", Desc: "POST a public key to /auth/dev/enroll"},
	{Name: "whoami", Desc: "Show current identity (signed call)"},
	{Name: "rotate-key", Desc: "Generate a new signing key for the active profile"},
	{Name: "revoke-key", Desc: "Revoke a signing key"},
	{Name: "revoke-actor", Desc: "Revoke an actor (cascades to all its keys)"},
	{Name: "invite", Desc: "Mint a signed teammate invitation"},
	{Name: "invitations", Desc: "List active invitations"},
	{Name: "revoke-invitation", Desc: "Revoke a pending invitation"},
	{Name: "accept", Desc: "Accept an invitation token"},
	{Name: "profiles", Desc: "List configured profiles"},
	{
		Name: "profile", Desc: "Profile subcommands",
		Children: []node{
			{Name: "use", Desc: "Set the active profile"},
			{Name: "show", Desc: "Show a profile's meta"},
			{Name: "remove", Desc: "Delete a profile's meta + key"},
		},
	},
	{Name: "tenants", Desc: "List tenants visible to the current actor"},
	{
		Name: "tenant", Desc: "Tenant subcommands",
		Children: []node{
			{Name: "create", Desc: "Create a new tenant"},
			{Name: "members", Desc: "List members of a tenant"},
			{Name: "grant", Desc: "Grant capabilities to an actor", Aliases: []string{"update-caps"}},
			{Name: "revoke", Desc: "Revoke an actor's tenant access"},
			{
				Name: "hostnames", Desc: "Per-tenant hostname routing",
				Children: []node{
					{Name: "list", Desc: "List hostnames bound to this tenant"},
					{Name: "add", Desc: "Claim a hostname (optionally bind to a stack)"},
					{Name: "attach", Desc: "Bind a claimed hostname to a stack"},
					{Name: "remove", Desc: "Revoke a hostname binding", Aliases: []string{"rm"}},
					{Name: "challenge", Desc: "Issue a proof-of-ownership challenge"},
					{Name: "verify", Desc: "Verify a hostname via its active challenge"},
					{Name: "status", Desc: "Show hostname binding status", Aliases: []string{"show"}},
				},
			},
			{
				Name: "secrets", Desc: "Per-tenant secret store",
				Children: []node{
					{Name: "set", Desc: "Store an operator-supplied secret value"},
					{Name: "generate", Desc: "Generate + store a random secret", Aliases: []string{"gen"}},
					{Name: "rotate", Desc: "Rotate a secret's value"},
					{Name: "list", Desc: "List secrets for this tenant", Aliases: []string{"ls"}},
					{Name: "show", Desc: "Reveal a secret's value (audited)"},
					{Name: "describe", Desc: "Update a secret's description"},
					{Name: "revoke", Desc: "Revoke a secret", Aliases: []string{"rm"}},
				},
			},
		},
	},
	{Name: "memberships", Desc: "Show this actor's memberships"},
	{Name: "logout", Desc: "Clear the active profile binding"},
	{Name: "login", Desc: "Open a browser session"},
	{
		Name: "sessions", Desc: "Browser session management",
		Children: []node{
			{Name: "list", Desc: "List active browser sessions"},
			{Name: "revoke", Desc: "Revoke a browser session"},
		},
	},
	{
		Name: "secrets", Desc: "Operator master-key management",
		Children: []node{
			{Name: "init", Desc: "Generate a master key"},
		},
	},
}

// opChildren mirrors the op dispatcher's switch at
// chassis/cli/op/dispatch.go:130-526.
var opChildren = []node{
	{Name: "init", Desc: "Scaffold a new op under OPS/<stack>/<scope>/<name>"},
	{Name: "build", Desc: "Build an op artifact (js/ts → wasm or txcl → bundle)"},
	{Name: "run", Desc: "Run an op locally with --input"},
	{Name: "test", Desc: "Run an op's mocks"},
}

// packageChildren mirrors chassis/cli/package_cmd.go.
var packageChildren = []node{
	{Name: "init", Desc: "Scaffold a package manifest"},
	{Name: "validate", Desc: "Validate a package manifest"},
	{Name: "inspect", Desc: "Print a package manifest"},
	{Name: "pull", Desc: "Pull a package from an OCI registry"},
	{Name: "publish", Desc: "Publish a package to an OCI registry"},
	{Name: "list", Desc: "List installed packages"},
	{Name: "upgrade", Desc: "Upgrade an installed package"},
	{Name: "remove", Desc: "Remove an installed package"},
	{
		Name: "key", Desc: "Package signing keys",
		Children: []node{
			{Name: "generate", Desc: "Generate a package signing key"},
		},
	},
}

// dnsChildren mirrors chassis/cli/dns.go.
var dnsChildren = []node{
	{Name: "render", Desc: "Render a zone file from chassis state"},
	{
		Name: "zone", Desc: "DNS zone management",
		Children: []node{
			{Name: "create", Desc: "Create a delegated zone"},
			{Name: "list", Desc: "List configured zones"},
			{Name: "delete", Desc: "Delete a zone"},
		},
	},
	{
		Name: "record", Desc: "DNS record overrides",
		Children: []node{
			{Name: "add", Desc: "Add a record override"},
			{Name: "list", Desc: "List record overrides"},
			{Name: "rm", Desc: "Remove a record override"},
		},
	},
	{
		Name: "config", Desc: "DNS chassis config",
		Children: []node{
			{Name: "show", Desc: "Show DNS config"},
			{Name: "set", Desc: "Update DNS config"},
		},
	},
}

// snapshotChildren mirrors chassis/cli/snapshot.go.
var snapshotChildren = []node{
	{Name: "export", Desc: "Export a chassis-state snapshot"},
	{Name: "import", Desc: "Import a chassis-state snapshot"},
	{Name: "publish", Desc: "Publish a snapshot to an artifact store"},
}

// configChildren is the `txco config <sub>` shortcut namespace, which
// forwards into auth. Mirrors chassis/cli/config.go.
var configChildren = []node{
	{Name: "profile", Desc: "Profile subcommands (alias for auth profile)"},
	{Name: "profiles", Desc: "List profiles (alias for auth profiles)"},
	{Name: "tenants", Desc: "List tenants (alias for auth tenants)"},
	{Name: "tenant", Desc: "Tenant subcommands (alias for auth tenant)"},
	{Name: "memberships", Desc: "Show memberships (alias for auth memberships)"},
	{Name: "logout", Desc: "Clear active profile (alias for auth logout)"},
}

// mcpChildren mirrors chassis/cli/mcp.go.
var mcpChildren = []node{
	{Name: "doctor", Desc: "Diagnose an MCP endpoint"},
}

// cloudChildren mirrors the Dispatch switch in chassis/cli/cloud/cloud.go.
// The same verbs are also exposed top-level (`txco login` / `txco logout`).
var cloudChildren = []node{
	{Name: "login", Desc: "Sign in to the thanks-computer cloud"},
	{Name: "logout", Desc: "Delete stored cloud tokens"},
	{Name: "whoami", Desc: "Show the current cloud identity", Aliases: []string{"status"}},
	{Name: "enroll", Desc: "Enroll a chassis key for the signed-in cloud profile"},
}

// adminTenantChildren mirrors runAdminTenant in chassis/cli/admin.go.
var adminTenantChildren = []node{
	{Name: "suspend", Desc: "Deny a tenant's requests until resumed"},
	{Name: "resume", Desc: "Restore a suspended tenant"},
}

// adminChildren mirrors the Dispatch switch in chassis/cli/admin.go:runAdmin.
var adminChildren = []node{
	{Name: "resync", Desc: "Re-emit a tenant's control-plane state to the fleet"},
	{Name: "tenant", Desc: "Suspend/resume a tenant's request admission", Children: adminTenantChildren},
}

// cliCommandTree is the authoritative root. Every non-hidden top-level
// case arm in chassis/cli/cli.go:Dispatch has an entry here. Order
// matches the printUsage block for visual consistency. Hidden aliases
// (e.g., `push` → `draft`) are NOT listed; the drift test allowlist
// (completion_test.go) names them.
var cliCommandTree = []node{
	{Name: "serve", Desc: "Run the chassis server"},
	{Name: "init", Desc: "Scaffold a new stack"},
	{Name: "apply", Desc: "Push the local workspace to a chassis"},
	{Name: "diff", Desc: "Show pending workspace changes vs chassis"},
	{Name: "status", Desc: "Show local workspace + chassis status"},
	{Name: "pull", Desc: "Fetch a stack's rules from the chassis"},
	{Name: "draft", Desc: "Push a draft version of a stack"},
	{Name: "activate", Desc: "Activate a draft version"},
	{Name: "versions", Desc: "List stack versions"},
	{Name: "edit", Desc: "Edit a rule and apply on save"},
	{Name: "dev", Desc: "Run the dev loop (apps + chassis + hot reload)"},
	{Name: "demo", Desc: "Start the txcl learning environment"},
	{Name: "trace", Desc: "Inspect request traces"},
	{Name: "snapshot", Desc: "Snapshot subcommands", Children: snapshotChildren},
	{Name: "auth", Desc: "Auth + identity management", Children: authChildren},
	{Name: "login", Desc: "Sign in to the thanks-computer cloud"},
	{Name: "logout", Desc: "Sign out of the thanks-computer cloud"},
	{Name: "cloud", Desc: "Cloud account subcommands (login/logout/whoami)", Children: cloudChildren},
	{Name: "op", Desc: "Op subcommands (scaffold/build/run/test)", Children: opChildren},
	{Name: "install", Desc: "Install a stack from a source"},
	{Name: "package", Desc: "Package subcommands (OCI distribution)", Children: packageChildren},
	{Name: "packages", Desc: "Alias for 'package list'"},
	{Name: "mcp", Desc: "MCP subcommands", Children: mcpChildren},
	{Name: "config", Desc: "Config shortcuts (alias namespace for auth)", Children: configChildren},
	{Name: "dns", Desc: "DNS zone + record management", Children: dnsChildren},
	{Name: "admin", Desc: "Operator-facing chassis maintenance", Children: adminChildren},
	{Name: "completion", Desc: "Emit shell completion script (bash|zsh|fish)"},
	{Name: "help", Desc: "Show top-level help"},
	{Name: "version", Desc: "Print version + build info"},
}

// --- runCompletion: dispatcher for `txco completion <shell>` ---

func runCompletion(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printCompletionHelp(stdout)
		return 0
	}
	switch args[0] {
	case "bash":
		fmt.Fprint(stdout, emitBash(cliCommandTree))
		return 0
	case "zsh":
		fmt.Fprint(stdout, emitZsh(cliCommandTree))
		return 0
	case "fish":
		fmt.Fprint(stdout, emitFish(cliCommandTree))
		return 0
	default:
		fmt.Fprintf(stderr, "completion: unknown shell %q (try: bash, zsh, fish)\n\n", args[0])
		printCompletionHelp(stderr)
		return 2
	}
}

func printCompletionHelp(w io.Writer) {
	fmt.Fprint(w, `Usage: txco completion <shell>

Emits a shell-completion script to stdout. Re-run after txco upgrades
so completion stays in sync with new commands.

  bash:  source <(txco completion bash)                       # current shell
         txco completion bash > /usr/local/etc/bash_completion.d/txco
                                                              # persistent (macOS, Homebrew)
         txco completion bash > /etc/bash_completion.d/txco   # persistent (Linux)

  zsh:   source <(txco completion zsh)                        # current shell
         txco completion zsh > "${fpath[1]}/_txco"            # persistent (uses your $fpath)

  fish:  txco completion fish > ~/.config/fish/completions/txco.fish
                                                              # persistent (auto-loaded)
`)
}

// --- emitters ---

// emitBash produces a bash completion function registered with
// `complete -F _txco txco`. The function inspects $COMP_WORDS to find
// the active dispatcher node and emits its children. Bash completion
// doesn't render per-candidate descriptions natively, so descriptions
// are dropped — just names.
func emitBash(tree []node) string {
	var b strings.Builder
	b.WriteString("# txco bash completion. Source this in your bash shell.\n")
	b.WriteString("# One-shot install: source <(txco completion bash)\n")
	b.WriteString("# Persistent:        txco completion bash > /etc/bash_completion.d/txco\n\n")
	b.WriteString("_txco() {\n")
	b.WriteString("    local cur cword words\n")
	b.WriteString("    if declare -F _init_completion >/dev/null; then\n")
	b.WriteString("        _init_completion || return\n")
	b.WriteString("    else\n")
	b.WriteString("        # Minimal fallback when bash-completion is not installed.\n")
	b.WriteString("        cur=\"${COMP_WORDS[COMP_CWORD]}\"\n")
	b.WriteString("        cword=$COMP_CWORD\n")
	b.WriteString("        words=(\"${COMP_WORDS[@]}\")\n")
	b.WriteString("    fi\n")
	b.WriteString("    local path=\"${words[*]:1:$((cword-1))}\"\n")
	b.WriteString("    case \"$path\" in\n")

	walk := func(prefix string, children []node) {}
	walk = func(prefix string, children []node) {
		// Build candidate list at this prefix.
		names := candidateNames(children)
		b.WriteString(fmt.Sprintf("        %q)\n", prefix))
		b.WriteString(fmt.Sprintf("            COMPREPLY=( $(compgen -W %q -- \"$cur\") )\n", strings.Join(names, " ")))
		b.WriteString("            return\n            ;;\n")
		// Recurse for each child that has its own children.
		for _, c := range children {
			if len(c.Children) == 0 {
				continue
			}
			sub := strings.TrimSpace(prefix + " " + c.Name)
			walk(sub, c.Children)
			// Also offer the canonical alias paths so a user who typed
			// `auth tenant secrets ls` still gets completion via the
			// alias (paths are bash-case literals).
			for _, al := range c.Aliases {
				_ = al // aliases at leaf level; dispatcher aliases are rare and we keep the canonical path
			}
		}
	}
	walk("", tree)

	b.WriteString("        *)\n")
	b.WriteString("            if [[ \"$cur\" == --* ]]; then\n")
	b.WriteString(fmt.Sprintf("                COMPREPLY=( $(compgen -W %q -- \"$cur\") )\n",
		strings.Join(commonFlags, " ")))
	b.WriteString("            fi\n")
	b.WriteString("            ;;\n")
	b.WriteString("    esac\n")
	b.WriteString("}\n")
	b.WriteString("complete -F _txco txco\n")
	return b.String()
}

// emitZsh produces a #compdef-style zsh script. Zsh renders per-
// candidate descriptions in its menu — the user's "shows us help"
// framing — so descriptions are passed through as `name[description]`.
func emitZsh(tree []node) string {
	var b strings.Builder
	b.WriteString("#compdef txco\n")
	b.WriteString("# txco zsh completion. Source this in your zsh shell.\n")
	b.WriteString("# One-shot install: source <(txco completion zsh)\n")
	b.WriteString("# Persistent:        txco completion zsh > \"${fpath[1]}/_txco\"\n\n")
	b.WriteString("_txco() {\n")
	b.WriteString("    local context state line\n")
	b.WriteString("    _arguments -C '1:command:->cmd' '*::arg:->args'\n")
	b.WriteString("    case $state in\n")
	b.WriteString("        cmd) _values 'txco command' \\\n")
	for i, n := range tree {
		sep := " \\\n"
		if i == len(tree)-1 {
			sep = "\n"
		}
		b.WriteString(fmt.Sprintf("            %s%s", zshValue(n), sep))
	}
	b.WriteString("            ;;\n")
	b.WriteString("        args)\n")
	b.WriteString("            case $line[1] in\n")
	// One sub-handler call per dispatcher child.
	for _, n := range tree {
		if len(n.Children) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("                %s) _txco_%s ;;\n", n.Name, zshFunc(n.Name)))
		// Also route aliases to the same handler.
		for _, al := range n.Aliases {
			b.WriteString(fmt.Sprintf("                %s) _txco_%s ;;\n", al, zshFunc(n.Name)))
		}
	}
	b.WriteString("            esac\n")
	b.WriteString("            ;;\n")
	b.WriteString("    esac\n")
	b.WriteString("}\n\n")

	// Emit one helper function per dispatcher node, depth-first.
	var emitFn func(path []string, children []node)
	emitFn = func(path []string, children []node) {
		fnName := "_txco_" + strings.Join(zshFuncs(path), "_")
		b.WriteString(fmt.Sprintf("%s() {\n", fnName))
		b.WriteString("    local context state line\n")
		b.WriteString("    _arguments -C '1:command:->cmd' '*::arg:->args'\n")
		b.WriteString("    case $state in\n")
		b.WriteString(fmt.Sprintf("        cmd) _values '%s subcommand' \\\n", strings.Join(path, " ")))
		for i, c := range children {
			sep := " \\\n"
			if i == len(children)-1 {
				sep = "\n"
			}
			b.WriteString(fmt.Sprintf("            %s%s", zshValue(c), sep))
		}
		b.WriteString("            ;;\n")
		// Dispatch to deeper helpers.
		hasNested := false
		for _, c := range children {
			if len(c.Children) > 0 {
				hasNested = true
				break
			}
		}
		if hasNested {
			b.WriteString("        args)\n")
			b.WriteString("            case $line[1] in\n")
			for _, c := range children {
				if len(c.Children) == 0 {
					continue
				}
				subFn := "_txco_" + strings.Join(zshFuncs(append(path, c.Name)), "_")
				b.WriteString(fmt.Sprintf("                %s) %s ;;\n", c.Name, subFn))
				for _, al := range c.Aliases {
					b.WriteString(fmt.Sprintf("                %s) %s ;;\n", al, subFn))
				}
			}
			b.WriteString("            esac\n")
			b.WriteString("            ;;\n")
		}
		b.WriteString("    esac\n")
		b.WriteString("}\n\n")
		for _, c := range children {
			if len(c.Children) > 0 {
				emitFn(append(path, c.Name), c.Children)
			}
		}
	}
	for _, n := range tree {
		if len(n.Children) == 0 {
			continue
		}
		emitFn([]string{n.Name}, n.Children)
	}

	// Register the completion function. Required when this script is
	// sourced into an interactive shell (`source <(txco completion zsh)`):
	// `_arguments` only works inside a completion context, so we cannot
	// just invoke `_txco` here. `compdef` is the right primitive — it
	// tells zsh "use this function to complete `txco`." When the file is
	// autoloaded from $fpath instead (saved as a `_txco` file in a
	// $fpath directory), the `#compdef txco` directive at the top is
	// what zsh consults; the compdef call below is a harmless idempotent
	// re-registration in that path.
	b.WriteString("if (( $+functions[compdef] )); then\n")
	b.WriteString("    compdef _txco txco\n")
	b.WriteString("fi\n")
	return b.String()
}

// emitFish produces line-based fish completion. Fish renders
// descriptions natively via -d.
func emitFish(tree []node) string {
	var b strings.Builder
	b.WriteString("# txco fish completion. Install: txco completion fish > ~/.config/fish/completions/txco.fish\n\n")
	for _, n := range tree {
		b.WriteString(fmt.Sprintf("complete -c txco -n %q -a %q -d %q\n",
			"__fish_use_subcommand", n.Name, sanitizeDesc(n.Desc)))
		for _, al := range n.Aliases {
			b.WriteString(fmt.Sprintf("complete -c txco -n %q -a %q -d %q\n",
				"__fish_use_subcommand", al, sanitizeDesc(n.Desc)+" (alias)"))
		}
	}
	// Walk nested dispatchers.
	var walk func(path []string, children []node)
	walk = func(path []string, children []node) {
		// Fish's __fish_seen_subcommand_from takes a space-separated
		// list of subcommands that should already be on the command
		// line, in any order. For multi-level chains we want a path
		// like "auth tenant secrets" → next word offers set/generate/…
		// We approximate this by checking ALL ancestor names are seen
		// AND none of the OTHER siblings are on the line.
		condition := fmt.Sprintf("__fish_seen_subcommand_from %s", strings.Join(path, " "))
		for _, c := range children {
			b.WriteString(fmt.Sprintf("complete -c txco -n %q -a %q -d %q\n",
				condition, c.Name, sanitizeDesc(c.Desc)))
			for _, al := range c.Aliases {
				b.WriteString(fmt.Sprintf("complete -c txco -n %q -a %q -d %q\n",
					condition, al, sanitizeDesc(c.Desc)+" (alias)"))
			}
		}
		for _, c := range children {
			if len(c.Children) > 0 {
				walk(append(path, c.Name), c.Children)
			}
		}
	}
	for _, n := range tree {
		if len(n.Children) > 0 {
			walk([]string{n.Name}, n.Children)
		}
	}
	return b.String()
}

// --- helpers ---

// candidateNames returns the bash-style space-separated candidate list
// for a slice of children. Both primary names and aliases are offered
// so a user who types a known alias gets correct continuation.
func candidateNames(children []node) []string {
	out := make([]string, 0, len(children))
	for _, c := range children {
		out = append(out, c.Name)
		out = append(out, c.Aliases...)
	}
	sort.Strings(out)
	return out
}

// zshValue formats one node as a zsh `_values` entry of the form
// `name[description]`. Descriptions are sanitized via sanitizeDesc
// (backticks → apostrophes — backticks inside the double-quoted entry
// would trigger zsh command substitution at script-load time, which
// hit us with `Alias for `package list``). Right-square-brackets are
// escaped so the parser doesn't think the description ended early.
func zshValue(n node) string {
	desc := sanitizeDesc(n.Desc)
	desc = strings.ReplaceAll(desc, `]`, `\]`)
	return fmt.Sprintf("%q", n.Name+"["+desc+"]")
}

// sanitizeDesc strips shell-hazardous characters from a node
// description so it's safe to embed in any of the three emitters'
// quoted strings. Concretely: backticks become apostrophes (avoids
// zsh command substitution inside "..."), and dollar signs become
// literal `\$` (avoids zsh parameter expansion). Both are defensive
// — every current description should already be safe — but a future
// addition that introduces one would otherwise reach a user before
// the test catches it.
func sanitizeDesc(s string) string {
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, `$`, `\$`)
	return s
}

// zshFunc maps a single command name to its zsh-function-safe form.
// Hyphens are replaced with underscores; everything else passes
// through.
func zshFunc(name string) string {
	return strings.ReplaceAll(name, "-", "_")
}

// zshFuncs maps a path slice through zshFunc.
func zshFuncs(path []string) []string {
	out := make([]string, len(path))
	for i, p := range path {
		out[i] = zshFunc(p)
	}
	return out
}
