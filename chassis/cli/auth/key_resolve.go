package auth

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
)

// ExistingEnrolmentKind classifies the disposition of an on-disk meta
// at the bootstrap-local target name + URL. Bootstrap dispatches on
// this value to decide whether to silently succeed, prompt for
// recovery, or bail with a network error.
type ExistingEnrolmentKind int

const (
	// EnrolmentNone means there's no usable meta — first-time setup
	// or a previous removal. Caller proceeds with normal bootstrap.
	EnrolmentNone ExistingEnrolmentKind = iota
	// EnrolmentValid means the existing key authenticates against
	// the target chassis right now. Caller exits 0 — the user is
	// already set up, no further action needed.
	EnrolmentValid
	// EnrolmentRejected means the meta is for the same chassis but
	// the key was refused (401/403). Likely a rebuilt chassis or
	// revoked key. Caller offers recovery.
	EnrolmentRejected
	// EnrolmentOtherChassis means the meta is for a different
	// chassis URL than the one bootstrap is targeting. Caller
	// offers recovery without trying the key.
	EnrolmentOtherChassis
	// EnrolmentUnreachable means we couldn't reach the chassis to
	// verify the key (network error, 5xx). Caller bails with the
	// underlying error rather than guessing.
	EnrolmentUnreachable
)

// classifyExistingEnrolment inspects an existing meta on disk and,
// when the URLs match, probes /auth/whoami to decide whether the
// stored key is still good. The new bootstrap-local UX dispatches
// on the returned kind:
//
//	EnrolmentNone         → first-time setup; proceed.
//	EnrolmentValid        → already signed in; exit 0 silently.
//	EnrolmentRejected     → key doesn't work here; recovery prompt.
//	EnrolmentOtherChassis → wrong chassis; recovery prompt.
//	EnrolmentUnreachable  → network error; bail with err.
//
// `meta` is non-nil for every kind except EnrolmentNone. `probeErr`
// is set only for EnrolmentUnreachable.
func classifyExistingEnrolment(name, urlFlag string, probe func(target client.Target) (ok bool, err error)) (kind ExistingEnrolmentKind, meta *Meta, probeErr error) {
	if name == "" {
		name = defaultKeyName
	}
	metaPath, err := MetaPath(name)
	if err != nil {
		return EnrolmentNone, nil, nil
	}
	m, err := LoadMeta(metaPath)
	if err != nil {
		// No existing meta (or unreadable) — treat as first-time.
		// Any read-failure surface keeps the normal flow's error,
		// which is clearer than a preflight could produce.
		return EnrolmentNone, nil, nil
	}
	if m.ChassisURL != urlFlag {
		return EnrolmentOtherChassis, m, nil
	}
	// Same chassis: try the key.
	target, terr := buildSignedTarget(name, urlFlag)
	if terr != nil || target.Auth == nil {
		// Meta present but signer missing (key file gone, agent down).
		// That's effectively rejected — recovery is the right path.
		return EnrolmentRejected, m, nil
	}
	ok, perr := probe(target)
	if perr != nil {
		return EnrolmentUnreachable, m, perr
	}
	if ok {
		return EnrolmentValid, m, nil
	}
	return EnrolmentRejected, m, nil
}

// probeWhoami is the production probe — signs GET /auth/whoami with
// the target's configured key and reports whether the chassis
// accepted it. 401/403 maps to ok=false (key rejected); other
// failures surface as err so the caller can bail rather than guess.
//
// Timeout is short by design: this runs during a CLI startup path
// where a hanging chassis URL shouldn't keep the user waiting.
func probeWhoami(target client.Target) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.New(target).Whoami(ctx)
	if err == nil {
		return true, nil
	}
	var he *client.HTTPError
	if errors.As(err, &he) && (he.StatusCode == 401 || he.StatusCode == 403) {
		return false, nil
	}
	return false, err
}

// EnrollmentChoices is the set of flag values that drive
// resolveEnrollmentKey. Held as a struct so each command can fill
// it from its own FlagSet without a long function signature, and so
// new flags can land without rippling through callers.
type EnrollmentChoices struct {
	SSHAgent   bool   // --ssh-agent: force agent backend
	NoSSHAgent bool   // --no-ssh-agent: forbid agent backend even when available
	SSHKey     string // --ssh-key <path>: use this existing on-disk key
	NewKey     bool   // --new-key: generate a fresh key under $TXCO_HOME
	Name       string // --name: destination basename for new keys / meta file
	Label      string // --label: used to suggest a renamed key on collision (ssh-keygen-style prompt)
}

// EnrollmentKey is the resolved choice: which public key to send to
// the chassis, which backend will sign with it, and how the meta
// file should be filled out.
type EnrollmentKey struct {
	PublicKey   ed25519.PublicKey // raw 32 bytes — goes to /auth/dev/enroll
	KeySource   string            // SourceFile | SourceSSHAgent
	KeyPath     string            // file backend: absolute path; "" otherwise
	Fingerprint string            // SHA256:… ssh-keygen format for display

	// privKey holds a freshly-generated key that hasn't been
	// persisted yet. Set ONLY when the resolver minted the key
	// (--new-key path or the fall-through default). nil for
	// ssh-agent or pre-existing files.
	privKey ed25519.PrivateKey

	// persistPath is where privKey gets written if-and-only-if the
	// chassis successfully enrolls. Empty when privKey is nil.
	persistPath string

	// freshName is the chosen basename for a freshly-generated key
	// (post-rename-prompt). Lets the caller align the meta file's
	// name with the rename without re-prompting.
	freshName string

	// CommentSuggestion is a human-readable hint to default the
	// --label flag from when the user didn't pass one. For
	// ssh-agent keys this is the agent's comment for the key
	// (typically "user@host"); for ssh-key files it's the comment
	// embedded in the OpenSSH PEM (also "user@host" for keys
	// produced by ssh-keygen). Empty when no comment is available.
	CommentSuggestion string
}

// MetaName returns the name the caller should use for SaveMeta /
// MetaPath. Same as the input --name when no rename happened;
// reflects the user's choice from the collision-rename prompt when
// generateFreshKey had to ask. Returns "" for ssh-agent / existing-
// file backends where the caller's --name is authoritative.
func (e *EnrollmentKey) MetaName() string { return e.freshName }

// PersistFreshKey writes a freshly-generated key to disk. Caller
// invokes this AFTER the chassis has accepted the public half, so a
// failed enrolment doesn't leave a stray key file around. No-op
// when the resolver didn't generate a key (ssh-agent / existing-
// file flows have nothing to persist).
//
// comment goes into the .pub sidecar's third field, matching what
// `ssh-keygen -C "<comment>"` would produce. Callers typically pass
// the user's --label so a teammate later running `ssh-keygen -lf
// <path>.pub` sees a familiar identity string.
func (e *EnrollmentKey) PersistFreshKey(comment string) error {
	if e.privKey == nil {
		return nil
	}
	return SavePrivateKeyWithComment(e.persistPath, e.privKey, comment)
}

// CleanupOnFailure removes any freshly-generated artifact if
// enrolment fails after we'd already written the key. Mirrors the
// pre-pluggable cleanup that bootstrap.go did inline; safe to call
// unconditionally.
func (e *EnrollmentKey) CleanupOnFailure() {
	if e.persistPath != "" {
		_ = os.Remove(e.persistPath)
	}
}

// agentDialer is the injection seam tests use to swap in an
// in-memory agent.Agent. Production opens $SSH_AUTH_SOCK.
var agentDialer = func() (agent.Agent, error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, signer.ErrNoAgent
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %q: %v", signer.ErrNoAgent, sock, err)
	}
	return agent.NewClient(conn), nil
}

// userHomeDir is overridable for tests that need to point ~/.ssh
// somewhere predictable. Production reads $HOME via os.UserHomeDir.
var userHomeDir = os.UserHomeDir

// resolveEnrollmentKey picks the key the chassis will register and
// the backend that will sign with it. See docs/auth.md "Bring your
// own key" for the canonical precedence; the inline comments below
// trace the same logic.
//
// stdin/isTTY/stderr are injected so tests can drive the prompt
// paths without a real terminal.
func resolveEnrollmentKey(c EnrollmentChoices, stdin io.Reader, isTTY bool, stderr io.Writer) (*EnrollmentKey, error) {
	// Conflicting explicit flags: refuse early with a clear error
	// rather than letting one quietly win over the other.
	explicit := 0
	if c.SSHAgent {
		explicit++
	}
	if c.SSHKey != "" {
		explicit++
	}
	if c.NewKey {
		explicit++
	}
	if explicit > 1 {
		return nil, errors.New("conflicting key flags; pass at most one of --ssh-agent, --ssh-key, --new-key")
	}
	if c.SSHAgent && c.NoSSHAgent {
		return nil, errors.New("--ssh-agent and --no-ssh-agent are mutually exclusive")
	}

	switch {
	case c.SSHAgent:
		return chooseAgentKey(stdin, isTTY, stderr)
	case c.SSHKey != "":
		return chooseExistingFile(c.SSHKey, stderr)
	case c.NewKey:
		return generateFreshKey(c.Name, c.Label, stdin, isTTY, stderr)
	}

	// No explicit flag. Auto-precedence:
	//   ssh-agent (unless --no-ssh-agent)
	//   → ~/.ssh/id_ed25519 prompt on TTY
	//   → fresh keygen under $TXCO_HOME
	if !c.NoSSHAgent {
		ek, err := tryAutoAgent(stdin, isTTY, stderr)
		if err == nil {
			return ek, nil
		}
		// Not finding an agent or a matching key is not an error in
		// the auto path — we fall through. Surface other errors
		// (multi-key on non-TTY, agent reachable but listing failed)
		// so the user can intervene.
		if !errors.Is(err, signer.ErrNoAgent) && err != errAutoSkip {
			return nil, err
		}
	}

	if ek, err := tryDefaultSSHKey(stdin, isTTY, stderr); err == nil && ek != nil {
		return ek, nil
	} else if err != nil && err != errAutoSkip {
		return nil, err
	}

	return generateFreshKey(c.Name, c.Label, stdin, isTTY, stderr)
}

// errAutoSkip is the sentinel "this branch declined to pick a key
// but it wasn't a real failure; try the next branch." Used internal
// to the auto-precedence walk so we don't return half-baked
// EnrollmentKey{} values from helpers.
var errAutoSkip = errors.New("auto-detect declined")

// chooseAgentKey is the --ssh-agent (explicit) path. The caller
// explicitly asked for an agent key, so we don't second-guess with a
// confirmation prompt — the flag IS the confirmation. Returns
// errAutoSkip when the agent has zero Ed25519 identities (caller can
// fall through if it wants), ErrNoAgent when SSH_AUTH_SOCK is unset,
// and a hard error for multi-key-on-non-TTY. On TTY with multiple
// keys, prompts the user to pick.
//
// Always prints a "using ssh-agent key SHA256:… (comment)" status
// line so the user sees which key got bound, both for trust and for
// later cross-referencing with `ssh-add -l`.
func chooseAgentKey(stdin io.Reader, isTTY bool, stderr io.Writer) (*EnrollmentKey, error) {
	picked, err := selectAgentKey(stdin, isTTY, stderr)
	if err != nil {
		return nil, err
	}
	pub, _ := agentKeyToEd25519(picked)
	fp := signer.Fingerprint(pub)
	fmt.Fprintf(stderr, "using ssh-agent key %s  (%s)\n", fp, picked.Comment)
	return &EnrollmentKey{
		PublicKey:         pub,
		KeySource:         SourceSSHAgent,
		Fingerprint:       fp,
		CommentSuggestion: picked.Comment,
	}, nil
}

// tryAutoAgent is the wrapper the AUTO (no explicit flag) path
// calls. Unlike chooseAgentKey it prompts the user for explicit
// confirmation before binding the agent key — the user didn't ask
// for the agent path, so silent enrolment would surprise them. On
// non-TTY (CI) we proceed silently with a printed status line, since
// pipelines can't answer prompts.
//
// errAutoSkip is returned when the user declines the prompt OR when
// the agent isn't usable. The auto path's caller falls through to
// `~/.ssh/id_ed25519` and fresh keygen.
func tryAutoAgent(stdin io.Reader, isTTY bool, stderr io.Writer) (*EnrollmentKey, error) {
	picked, err := selectAgentKey(stdin, isTTY, stderr)
	if err != nil {
		if errors.Is(err, signer.ErrNoAgent) {
			return nil, signer.ErrNoAgent
		}
		return nil, err
	}
	pub, _ := agentKeyToEd25519(picked)
	fp := signer.Fingerprint(pub)

	if isTTY {
		// Don't surprise the user — they typed `txco auth
		// bootstrap-local`, not `--ssh-agent`. Show the fingerprint
		// AND the comment, then ask.
		prompt := fmt.Sprintf("use ssh-agent key %s  (%s)? [Y/n]: ", fp, picked.Comment)
		if !promptYesNo(stdin, stderr, prompt, true) {
			return nil, errAutoSkip
		}
	} else {
		fmt.Fprintf(stderr, "using ssh-agent key %s\n", fp)
	}

	return &EnrollmentKey{
		PublicKey:         pub,
		KeySource:         SourceSSHAgent,
		Fingerprint:       fp,
		CommentSuggestion: picked.Comment,
	}, nil
}

// selectAgentKey is the shared agent-key picker for both the
// explicit (--ssh-agent) and the auto paths. Returns the single
// matching agent.Key, or an appropriate error sentinel:
//
//   - signer.ErrNoAgent: socket unset / unreachable.
//   - errAutoSkip: agent reachable but has zero Ed25519 keys.
//   - hard error: multiple Ed25519 keys on non-TTY (the auto path
//     converts this back to errAutoSkip via its caller; the explicit
//     path surfaces it to the user).
//
// On TTY with multiple keys, prompts the user to pick by number.
func selectAgentKey(stdin io.Reader, isTTY bool, stderr io.Writer) (*agent.Key, error) {
	a, err := agentDialer()
	if err != nil {
		return nil, err
	}
	keys, err := a.List()
	if err != nil {
		return nil, fmt.Errorf("agent list: %w", err)
	}
	ed := filterEd25519(keys)
	switch len(ed) {
	case 0:
		return nil, fmt.Errorf("%w: agent reachable but has no Ed25519 identities (ssh-add ~/.ssh/id_ed25519)", signer.ErrNoAgent)
	case 1:
		return ed[0], nil
	default:
		if !isTTY {
			fmt.Fprintln(stderr, "ssh-agent has multiple Ed25519 keys:")
			for i, k := range ed {
				pub, _ := agentKeyToEd25519(k)
				fmt.Fprintf(stderr, "  [%d] %s  %s\n", i+1, signer.Fingerprint(pub), k.Comment)
			}
			return nil, errors.New("multiple ssh-agent identities; pick one with `ssh-add -D` + `ssh-add <path>`, or pass --new-key")
		}
		return promptPickAgentKey(ed, stdin, stderr)
	}
}

// chooseExistingFile is the --ssh-key path. Reads the file's
// public-key half (load + extract) so the chassis gets the right
// pubkey to register. No prompting; --ssh-key is explicit by
// construction.
//
// Tries to recover the SSH key comment from a sibling .pub file
// (the standard place — `ssh-keygen` writes "<type> <b64-pub>
// <comment>" there). Used to default --label so the actor row on
// the chassis carries something human-readable like
// "matt@laptop" instead of empty.
func chooseExistingFile(path string, stderr io.Writer) (*EnrollmentKey, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("--ssh-key %q: %w", path, err)
	}
	priv, err := signer.LoadEd25519PrivateKey(abs, true)
	if err != nil {
		return nil, fmt.Errorf("--ssh-key %q: %w", path, err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("--ssh-key %q is not an ed25519 key", path)
	}
	fp := signer.Fingerprint(pub)
	comment := readPubCommentBesides(abs)
	if comment != "" {
		fmt.Fprintf(stderr, "using key %s  (%s, %s)\n", fp, abs, comment)
	} else {
		fmt.Fprintf(stderr, "using key %s  (%s)\n", fp, abs)
	}
	return &EnrollmentKey{
		PublicKey:         pub,
		KeySource:         SourceFile,
		KeyPath:           abs,
		Fingerprint:       fp,
		CommentSuggestion: comment,
	}, nil
}

// readPubCommentBesides looks for "<privPath>.pub" — the canonical
// place ssh-keygen writes the public-key sidecar — and extracts the
// comment field. `ssh-keygen -t ed25519 -f foo` produces "foo.pub"
// shaped like:
//
//	ssh-ed25519 AAAA…(base64-blob) <comment>
//
// Returns "" when no sidecar exists, the file doesn't parse as an
// authorized_keys line, or the comment is empty. Best-effort —
// missing comment is normal, not an error.
//
// Why a sibling file instead of digging into the PEM: the OpenSSH
// private-key PEM format DOES embed a comment, but stdlib's
// crypto/ssh.ParseRawPrivateKey discards it and the structured
// fields aren't exported. The .pub sidecar is the supported public
// surface for recovering the comment.
func readPubCommentBesides(privPath string) string {
	raw, err := os.ReadFile(privPath + ".pub")
	if err != nil {
		return ""
	}
	_, comment, _, _, err := ssh.ParseAuthorizedKey(raw)
	if err != nil {
		return ""
	}
	return comment
}

// tryDefaultSSHKey is the auto-fall-through that scans ~/.ssh/ for
// Ed25519 private keys and lets the user pick one (or skip to fresh
// keygen). Non-TTY callers (CI) skip outright — a pipeline shouldn't
// silently bind a developer's SSH identity.
//
// The picker shows:
//   - one numbered entry per discovered Ed25519 key (private only;
//     .pub / config / known_hosts are ignored). Encrypted keys are
//     listed as "(encrypted)" — they'll prompt for a passphrase if
//     selected.
//   - an "enter another path" option for keys outside ~/.ssh/.
//   - a "skip" option that falls through to fresh keygen.
//
// Default selection is "skip" so just hitting Enter doesn't bind
// anything. After picking, the user gets a y/N confirmation that
// defaults to N — they have to actively type "y" to enrol an
// existing key.
func tryDefaultSSHKey(stdin io.Reader, isTTY bool, stderr io.Writer) (*EnrollmentKey, error) {
	if !isTTY {
		return nil, errAutoSkip
	}
	home, err := userHomeDir()
	if err != nil {
		return nil, errAutoSkip
	}
	sshDir := filepath.Join(home, ".ssh")
	candidates := scanSSHEd25519Keys(sshDir)
	if len(candidates) == 0 {
		return nil, errAutoSkip
	}

	// One bufio.Reader threaded through both prompts. Critical:
	// bufio reads ahead, so creating a fresh reader between the
	// picker and the confirmation would lose whatever the user
	// already typed for the second prompt — invisibly, with no
	// error. See promptYesNoReader's comment.
	reader := bufio.NewReader(stdin)

	picked, err := pickSSHCandidate(candidates, sshDir, home, reader, stderr)
	if err != nil || picked.path == "" {
		return nil, errAutoSkip
	}

	// If the user typed a non-existent path and opted to create a
	// new ed25519 there, this is a "fresh keygen, just at a
	// user-chosen path" — set up an EnrollmentKey whose
	// PersistFreshKey will write both the OpenSSH private key AND
	// the standard .pub sidecar, matching what `ssh-keygen -t
	// ed25519 -f <path>` would produce.
	if picked.createNew {
		return makeFreshFileEnrollmentKey(picked.path, stderr)
	}

	priv, err := signer.LoadEd25519PrivateKey(picked.path, true)
	if err != nil {
		fmt.Fprintf(stderr, "  (couldn't load %s: %v)\n", picked.path, err)
		return nil, errAutoSkip
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		fmt.Fprintf(stderr, "  (%s is not an ed25519 key)\n", picked.path)
		return nil, errAutoSkip
	}
	fp := signer.Fingerprint(pub)

	// Default-N confirmation. The user picked the key from a list,
	// but enrolling an SSH identity is non-reversible enough (the
	// chassis will record this pubkey forever) that an explicit
	// "yes" beats "press enter to enrol."
	if !promptYesNoReader(reader, stderr,
		fmt.Sprintf("enroll %s (%s)? [y/N]: ", picked.path, fp), false) {
		return nil, errAutoSkip
	}
	return &EnrollmentKey{
		PublicKey:         pub,
		KeySource:         SourceFile,
		KeyPath:           picked.path,
		Fingerprint:       fp,
		CommentSuggestion: readPubCommentBesides(picked.path),
	}, nil
}

// makeFreshFileEnrollmentKey generates a new ed25519 keypair to be
// persisted at the user-chosen path on successful enrolment.
// Mirrors the data shape of generateFreshKey (privKey + persistPath),
// so PersistFreshKey writes the key+sidecar only AFTER the chassis
// has confirmed enrolment — a failed enrol leaves no orphan files.
//
// Used when the user picks "[N] enter another path" with a path
// that doesn't exist yet, and opts in to "create here?". The
// resulting on-disk artifacts are byte-for-byte what `ssh-keygen
// -t ed25519 -f <path>` would write (OpenSSH PEM + .pub sidecar),
// so the file is fully usable by any other SSH tooling later.
func makeFreshFileEnrollmentKey(path string, stderr io.Writer) (*EnrollmentKey, error) {
	pub, priv, err := GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate: %w", err)
	}
	fp := signer.Fingerprint(pub)
	fmt.Fprintf(stderr, "  will create %s on successful enrolment  (%s)\n", path, fp)
	return &EnrollmentKey{
		PublicKey:   pub,
		KeySource:   SourceFile,
		KeyPath:     path,
		Fingerprint: fp,
		privKey:     priv,
		persistPath: path,
	}, nil
}

// sshKeyCandidate is one entry in the picker. fingerprint is "" for
// encrypted files (we can't compute it without unlocking), in which
// case the picker shows "(encrypted)" instead.
type sshKeyCandidate struct {
	path        string
	fingerprint string
	encrypted   bool
}

// scanSSHEd25519Keys walks dir for private-key files that parse as
// ed25519 (or are encrypted, which we can't disprove without a
// passphrase — include them and prompt later). Returns nil on an
// unreadable or missing dir; the caller treats that as
// "nothing to offer" and falls through.
func scanSSHEd25519Keys(dir string) []sshKeyCandidate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []sshKeyCandidate
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !looksLikePrivateKeyFile(name) {
			continue
		}
		path := filepath.Join(dir, name)
		priv, err := signer.LoadEd25519PrivateKey(path, false)
		if err != nil {
			// Encrypted? List as a candidate; the user can choose
			// to unlock it. Any other error → silently drop (not
			// our key, or unparseable).
			var pme *signer.PassphraseMissingError
			if errors.As(err, &pme) {
				out = append(out, sshKeyCandidate{path: path, encrypted: true})
			}
			continue
		}
		pub, ok := priv.Public().(ed25519.PublicKey)
		if !ok {
			continue
		}
		out = append(out, sshKeyCandidate{
			path:        path,
			fingerprint: signer.Fingerprint(pub),
		})
	}
	// Stable order: id_ed25519 first (the conventional default),
	// then alphabetical. Mirrors what `ls -1` would show with the
	// most-likely-intended key on top.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := filepath.Base(out[i].path), filepath.Base(out[j].path)
		if a == "id_ed25519" && b != "id_ed25519" {
			return true
		}
		if b == "id_ed25519" && a != "id_ed25519" {
			return false
		}
		return a < b
	})
	return out
}

// looksLikePrivateKeyFile is a cheap pre-filter so we don't try to
// parse every random file in ~/.ssh/ as a private key. Anything that
// matches a typical private-key naming convention AND doesn't end in
// .pub passes; everything else (config, known_hosts*, authorized_*,
// agent socket, etc.) is dropped.
func looksLikePrivateKeyFile(name string) bool {
	if strings.HasSuffix(name, ".pub") {
		return false
	}
	switch {
	case strings.HasPrefix(name, "id_"),
		strings.HasSuffix(name, ".ed25519"),
		strings.HasSuffix(name, "_ed25519"):
		return true
	}
	// Anything starting with "known_hosts" / "authorized_" / "config" /
	// "environment" / agent socket names falls through implicitly.
	return false
}

// pickSSHCandidate shows the numbered picker and returns the
// chosen path along with a createNew flag for the "type a path that
// doesn't exist" → "generate here" case. An empty path means "skip"
// (the user took the default or hit Enter at the manual-entry
// prompt).
//
// Supports:
//   - bare numbers selecting one of the discovered candidates
//     (existing files; createNew is always false).
//   - a "type a path" option that prompts for an arbitrary path
//     (with ~/ expansion). If the path doesn't exist, the user is
//     offered the choice to generate a fresh ed25519 there — that's
//     the SSH-keystore-drop-in flow.
//   - a "skip" option that's the default — pressing Enter at the
//     prompt falls through to fresh keygen under $TXCO_HOME.
//
// Invalid input (typo / out-of-range) falls through to skip rather
// than looping — the auto path would still produce a working key
// via fresh keygen, and an infinite-loop prompt would be hostile.
//
// Takes a *bufio.Reader (not io.Reader) because callers chain
// further prompts after this one and need the underlying buffer
// preserved. See promptYesNoReader's comment.
func pickSSHCandidate(candidates []sshKeyCandidate, sshDir, home string, reader *bufio.Reader, stderr io.Writer) (manualPathChoice, error) {
	fmt.Fprintf(stderr, "found Ed25519 keys under %s:\n", sshDir)
	for i, c := range candidates {
		if c.encrypted {
			fmt.Fprintf(stderr, "  [%d] %s  (encrypted)\n", i+1, c.path)
		} else {
			fmt.Fprintf(stderr, "  [%d] %s  %s\n", i+1, c.path, c.fingerprint)
		}
	}
	typeIdx := len(candidates) + 1
	skipIdx := len(candidates) + 2
	fmt.Fprintf(stderr, "  [%d] enter another path\n", typeIdx)
	fmt.Fprintf(stderr, "  [%d] skip (generate a fresh key under %s) [default]\n", skipIdx, HomePathPretty())
	fmt.Fprintf(stderr, "pick [%d]: ", skipIdx)

	line, readErr := reader.ReadString('\n')
	if readErr != nil && readErr != io.EOF {
		return manualPathChoice{}, fmt.Errorf("read selection: %w", readErr)
	}
	sel := strings.TrimSpace(line)
	if sel == "" {
		return manualPathChoice{}, nil // default: skip
	}
	n, err := strconv.Atoi(sel)
	if err != nil {
		fmt.Fprintln(stderr, "  invalid selection; skipping to fresh keygen")
		return manualPathChoice{}, nil
	}
	switch {
	case n == skipIdx:
		return manualPathChoice{}, nil
	case n == typeIdx:
		// Loop on bad input — the user chose this branch
		// explicitly ("enter another path"), so a typo should give
		// them another chance, not silently fall through to fresh
		// keygen. Empty input is the escape hatch: hit Enter to
		// skip back to "[default] skip" semantics. A non-existent
		// path triggers the "create here?" prompt.
		return promptForManualPath(home, reader, stderr)
	case n >= 1 && n <= len(candidates):
		return manualPathChoice{path: candidates[n-1].path}, nil
	default:
		fmt.Fprintln(stderr, "  out-of-range selection; skipping to fresh keygen")
		return manualPathChoice{}, nil
	}
}

// manualPathChoice is what promptForManualPath returns. createNew=true
// means the user asked us to generate a fresh key at this path; the
// caller will set up an EnrollmentKey whose persistPath is this path
// (so the key gets written to disk only after the chassis confirms
// enrolment, mirroring the --new-key flow).
type manualPathChoice struct {
	path      string // "" → user picked Enter-to-skip
	createNew bool   // true → caller generates + writes a fresh ed25519 here
}

// promptForManualPath is the "enter another path" sub-prompt. Loops
// until either:
//   - the user enters an existing path (returned as absolute, with
//     ~/ expansion applied), createNew=false.
//   - the user enters a path that doesn't yet exist, AND opts in to
//     "create a new ed25519 key here?" — returned with createNew=true.
//   - the user enters an empty line — escape hatch, path="" means
//     "skip back to fresh keygen under $TXCO_HOME."
//
// The create-new branch is what makes txco a drop-in for the SSH
// keystore: a user can enroll under a fresh path like
// ~/.ssh/id_ed25519-txco and end up with a working OpenSSH-format
// key + .pub sidecar that any other SSH tool can also consume.
func promptForManualPath(home string, reader *bufio.Reader, stderr io.Writer) (manualPathChoice, error) {
	for {
		fmt.Fprint(stderr, "path (Enter to skip): ")
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return manualPathChoice{}, fmt.Errorf("read path: %w", readErr)
		}
		p := strings.TrimSpace(line)
		if p == "" {
			return manualPathChoice{}, nil
		}
		// Expand ~ — the user types what they'd type at the shell.
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			fmt.Fprintf(stderr, "  invalid path %q; try again or hit Enter to skip\n", p)
			if errors.Is(readErr, io.EOF) {
				return manualPathChoice{}, nil
			}
			continue
		}

		// Path exists → use as-is. The caller will load it.
		if _, statErr := os.Stat(abs); statErr == nil {
			return manualPathChoice{path: abs}, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			fmt.Fprintf(stderr, "  cannot stat %s: %v\n", abs, statErr)
			if errors.Is(readErr, io.EOF) {
				return manualPathChoice{}, nil
			}
			continue
		}

		// Path doesn't exist. Two sub-options:
		//   (a) generate a fresh ed25519 key here. The destination
		//       has to be in a directory we can write to, so we
		//       require the parent directory to already exist
		//       (mkdir -p is sharp tooling for an auto-flow).
		//   (b) the user mistyped; back to the path prompt.
		parent := filepath.Dir(abs)
		if _, perr := os.Stat(parent); perr != nil {
			fmt.Fprintf(stderr, "  %s does not exist (parent dir %s is also missing); fix the path or hit Enter to skip\n", abs, parent)
			if errors.Is(readErr, io.EOF) {
				return manualPathChoice{}, nil
			}
			continue
		}
		fmt.Fprintf(stderr, "  %s does not exist.\n", abs)
		if !promptYesNoReader(reader, stderr,
			fmt.Sprintf("  create a new ed25519 key at %s? [Y/n]: ", abs), true) {
			// User declined creation; loop to ask for a different
			// path (or Enter to skip).
			continue
		}
		return manualPathChoice{path: abs, createNew: true}, nil
	}
}

// generateFreshKey produces a brand-new ed25519 key in memory and
// returns the path it will be persisted to. If the default name is
// already taken on disk, behaviour depends on the terminal:
//   - TTY: ssh-keygen-style rename prompt — "key X already exists.
//     key name [suggested]: " — loops until the user picks a free
//     name (or Ctrl-C aborts). Suggestion is derived from --label
//     when present, otherwise from the lowest free `local-N`.
//   - non-TTY: hard error, pointing the user at --name.
//
// The actual file write doesn't happen here; that's PersistFreshKey,
// called only after the chassis has confirmed enrolment.
func generateFreshKey(name, label string, stdin io.Reader, isTTY bool, stderr io.Writer) (*EnrollmentKey, error) {
	if name == "" {
		name = defaultKeyName
	}
	chosenName, keyPath, err := pickFreeKeyName(name, label, stdin, isTTY, stderr)
	if err != nil {
		return nil, err
	}
	pub, priv, err := GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("generate: %w", err)
	}
	return &EnrollmentKey{
		PublicKey:   pub,
		KeySource:   SourceFile,
		KeyPath:     keyPath,
		Fingerprint: signer.Fingerprint(pub),
		privKey:     priv,
		persistPath: keyPath,
		freshName:   chosenName,
	}, nil
}

// pickFreeKeyName resolves where to write a freshly-generated key
// when the user didn't pass --name explicitly:
//
//   - default name is free → use it silently.
//   - default taken + non-TTY → error pointing at --name.
//   - default taken + TTY → prompt with a suggested alternative;
//     loop on further collisions; accept the suggestion on empty
//     input (ssh-keygen UX).
//
// Returns the chosen basename + the absolute key file path it
// resolves to under $TXCO_HOME/keys/.
func pickFreeKeyName(initial, label string, stdin io.Reader, isTTY bool, stderr io.Writer) (string, string, error) {
	keyPath, err := KeyPath(initial)
	if err != nil {
		return "", "", err
	}
	if _, statErr := os.Stat(keyPath); errors.Is(statErr, os.ErrNotExist) {
		return initial, keyPath, nil
	} else if statErr != nil {
		return "", "", statErr
	}

	if !isTTY {
		return "", "", fmt.Errorf("%q already exists; pass --ssh-key to reuse it, --name <other> to pick a different name, or remove it first", keyPath)
	}

	fmt.Fprintf(stderr, "key %q already exists.\n", keyPath)
	suggested := suggestKeyName(label)
	reader := bufio.NewReader(stdin)
	for {
		fmt.Fprintf(stderr, "key name [%s]: ", suggested)
		line, readErr := reader.ReadString('\n')
		if readErr != nil && readErr != io.EOF {
			return "", "", fmt.Errorf("read name: %w", readErr)
		}
		name := strings.TrimSpace(line)
		if name == "" {
			name = suggested
		}
		if !validKeyName(name) {
			fmt.Fprintln(stderr, "  invalid name (use letters, digits, '_' or '-')")
			if errors.Is(readErr, io.EOF) {
				return "", "", errors.New("eof while choosing key name; pass --name to skip the prompt")
			}
			continue
		}
		candidate, err := KeyPath(name)
		if err != nil {
			return "", "", err
		}
		if _, sErr := os.Stat(candidate); errors.Is(sErr, os.ErrNotExist) {
			return name, candidate, nil
		} else if sErr != nil {
			return "", "", sErr
		}
		fmt.Fprintf(stderr, "  %q already exists, pick another\n", candidate)
		suggested = nextSuggestion(suggested)
		if errors.Is(readErr, io.EOF) {
			return "", "", errors.New("eof while choosing key name; pass --name to skip the prompt")
		}
	}
}

// filterEd25519 keeps only the Ed25519 entries from agent.Key list.
// Other algorithms (RSA, ECDSA) are silently dropped — the chassis
// only registers raw 32-byte ed25519 keys, so other types in the
// agent are irrelevant to us.
func filterEd25519(keys []*agent.Key) []*agent.Key {
	out := make([]*agent.Key, 0, len(keys))
	for _, k := range keys {
		if k.Format == ssh.KeyAlgoED25519 {
			out = append(out, k)
		}
	}
	return out
}

// agentKeyToEd25519 unpacks the SSH wire-format pubkey blob from an
// agent.Key entry into the raw 32-byte ed25519.PublicKey we send to
// the chassis. The SSH wire format is `string("ssh-ed25519") +
// string(32-byte-raw-pubkey)`; we parse via ssh.ParsePublicKey to
// keep the unmarshaling in one well-tested place.
func agentKeyToEd25519(k *agent.Key) (ed25519.PublicKey, error) {
	sshPub, err := ssh.ParsePublicKey(k.Marshal())
	if err != nil {
		return nil, fmt.Errorf("parse agent key: %w", err)
	}
	cpk, ok := sshPub.(ssh.CryptoPublicKey)
	if !ok {
		return nil, fmt.Errorf("agent key %q does not expose crypto.PublicKey", k.Comment)
	}
	pub, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("agent key %q is not ed25519 (got %T)", k.Comment, cpk.CryptoPublicKey())
	}
	return pub, nil
}

// promptPickAgentKey renders a numbered list and reads a 1-based
// selection. Loops on invalid input rather than failing fast, so a
// typo doesn't abort the whole enrolment.
func promptPickAgentKey(keys []*agent.Key, stdin io.Reader, stderr io.Writer) (*agent.Key, error) {
	fmt.Fprintln(stderr, "ssh-agent has multiple Ed25519 keys:")
	for i, k := range keys {
		pub, _ := agentKeyToEd25519(k)
		fmt.Fprintf(stderr, "  [%d] %s  %s\n", i+1, signer.Fingerprint(pub), k.Comment)
	}
	reader := bufio.NewReader(stdin)
	for {
		fmt.Fprint(stderr, "pick one [1]: ")
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("read pick: %w", err)
		}
		s := strings.TrimSpace(line)
		if s == "" {
			s = "1"
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > len(keys) {
			fmt.Fprintln(stderr, "  enter a number from the list")
			if errors.Is(err, io.EOF) {
				return nil, errors.New("eof while picking agent key")
			}
			continue
		}
		return keys[n-1], nil
	}
}

// promptYesNo reads a y/n line from stdin. `def` is the value used
// when the user hits enter with no input — mirrors the typical
// `[Y/n]` vs `[y/N]` shell convention.
//
// Builds its own bufio.Reader; safe to use as a one-shot when no
// other prompt shares the same stdin instance in this call. When a
// caller chains multiple prompts (numbered picker + y/N confirm),
// use promptYesNoReader with a SHARED *bufio.Reader to avoid losing
// buffered input between calls — bufio.NewReader reads ahead and
// the next reader can't recover the residue.
func promptYesNo(stdin io.Reader, stderr io.Writer, prompt string, def bool) bool {
	return promptYesNoReader(bufio.NewReader(stdin), stderr, prompt, def)
}

// promptYesNoReader is the shared-reader variant of promptYesNo. Use
// this when the caller is already inside a sequence of stdin reads
// and needs to keep the buffer consistent across them.
func promptYesNoReader(r *bufio.Reader, stderr io.Writer, prompt string, def bool) bool {
	fmt.Fprint(stderr, prompt)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return def
	}
	s := strings.ToLower(strings.TrimSpace(line))
	if s == "" {
		return def
	}
	return s == "y" || s == "yes"
}
