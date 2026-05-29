package auth

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// runSecretsCmd dispatches `txco auth secrets <sub>`. PR 1 of the
// per-tenant secret store ships only the `init` verb. PR 4 will fill
// in tenant-scoped CRUD (`set`, `generate`, `rotate`, `list`, …)
// under `txco auth tenant secrets <sub>`.
//
// Signature mirrors the other auth sub-dispatchers (no stdin) so the
// auth.Dispatch caller stays unchanged. Verbs that need stdin (like
// the --force confirmation in `init`) read os.Stdin internally;
// tests call the underlying `runSecretsInit` with an explicit stdin.
//
// See internal docs/todo-secret-store.md (design) and
// internal docs/todo-secret-store-implementation.md (implementation arc).
func runSecretsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printSecretsUsage(stdout)
		return 0
	}
	switch args[0] {
	case "init":
		return runSecretsInit(args[1:], os.Stdin, stdout, stderr)
	case "help", "-h", "--help":
		printSecretsUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "auth secrets: unknown subcommand %q\n\n", args[0])
		printSecretsUsage(stderr)
		return 2
	}
}

func printSecretsUsage(w io.Writer) {
	banner.PrintLogo(w)
	fmt.Fprint(w, `
Usage:
  txco auth secrets [flags]
  txco auth secrets <command> [flags]

Manage the host-local master key that protects the per-tenant secret
store. The master key is operator-owned, lives outside the database,
and is required at chassis boot for the secret store to function.

Available commands:
  init    Mint a fresh 32-byte master key at --path (or $TXCO_SECRET_MASTER_KEY)

Examples:
  # Mint the master key (production).
  txco auth secrets init --path /data/secrets/txco-master.key

  # Rotate (rare — irrecoverably invalidates every stored secret
  # until re-encrypted). Requires confirmation.
  txco auth secrets init --path /data/secrets/txco-master.key --force

After minting, set --secret-master-key=<path> on the chassis (or
TXCO_SECRET_MASTER_KEY=<path> in its environment).

CAUTION: lose the master-key file and every stored secret becomes
unrecoverable. Back it up separately from the database.
`)
}

func runSecretsInit(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("auth secrets init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("path", "", "where to write the master key file (required; falls back to $TXCO_SECRET_MASTER_KEY)")
	force := fs.Bool("force", false, "overwrite an existing file (requires interactive 'overwrite' confirmation)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco auth secrets init --path PATH [--force]

Mint a fresh 32-byte master key at PATH (file mode 0600).
By default refuses if PATH already exists; --force prompts for
interactive confirmation before replacing.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Resolve target path: --path > env > error.
	target := *path
	if target == "" {
		target = os.Getenv("TXCO_SECRET_MASTER_KEY")
	}
	if target == "" {
		fmt.Fprintln(stderr, "auth secrets init: --path is required (or set $TXCO_SECRET_MASTER_KEY)")
		return 2
	}

	// Handle existing path. Three cases:
	//   1. Path exists and is a directory ⇒ --path is wrong (it's a
	//      file path, not a directory). Reject with a useful hint;
	//      --force MUST NOT delete a directory to write a file at
	//      the same name (the operator almost certainly wanted to
	//      write a key file INSIDE the directory).
	//   2. Path exists and is a regular file ⇒ refuse unless --force;
	//      --force prompts for "overwrite" confirmation. This is
	//      the rotation path (rare; design §3).
	//   3. Path doesn't exist ⇒ fall through to MintFileMasterKey,
	//      which creates parent dirs as needed.
	if info, err := os.Stat(target); err == nil {
		if info.IsDir() {
			suggested := strings.TrimRight(target, "/") + "/txco-master.key"
			fmt.Fprintf(stderr, "auth secrets init: %q is a directory; --path must be a file path.\n", target)
			fmt.Fprintf(stderr, "did you mean: --path %s\n", suggested)
			return 1
		}
		if !info.Mode().IsRegular() {
			fmt.Fprintf(stderr, "auth secrets init: %q is not a regular file (mode=%s); refusing to overwrite\n",
				target, info.Mode())
			return 1
		}
		if !*force {
			fmt.Fprintf(stderr, "auth secrets init: %q already exists; pass --force to replace\n", target)
			return 1
		}
		fmt.Fprintf(stdout, "WARNING: replacing the master key will permanently invalidate\n")
		fmt.Fprintf(stdout, "         every secret encrypted under the existing key. Back up\n")
		fmt.Fprintf(stdout, "         %q first if any tenant has live secrets.\n\n", target)
		fmt.Fprint(stdout, `Type "overwrite" to confirm: `)
		if !readConfirmation(stdin, "overwrite") {
			fmt.Fprintln(stdout, "aborted.")
			return 1
		}
		if err := os.Remove(target); err != nil {
			fmt.Fprintf(stderr, "auth secrets init: remove existing key: %v\n", err)
			return 1
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "auth secrets init: stat %q: %v\n", target, err)
		return 1
	}

	if err := secrets.MintFileMasterKey(target); err != nil {
		fmt.Fprintf(stderr, "auth secrets init: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "wrote master key to %s (32 bytes, 0600 perms)\n", target)
	fmt.Fprintln(stdout, "next: set --secret-master-key on the chassis (or TXCO_SECRET_MASTER_KEY in its env).")
	return 0
}

// readConfirmation reads a single line from stdin and reports
// whether it (trimmed) matches want. Used for the --force gate.
func readConfirmation(stdin io.Reader, want string) bool {
	if stdin == nil {
		return false
	}
	br := bufio.NewReader(stdin)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	return strings.TrimSpace(line) == want
}
