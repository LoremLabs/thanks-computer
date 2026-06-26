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
	SSHAgent bool   // --ssh-agent: force agent backend
	SSHKey   string // --ssh-key <path>: use this existing on-disk key
	NewKey   bool   // --new-key: generate a fresh key under $TXCO_HOME
	Name     string // --name: destination basename for new keys / meta file
	Label    string // --label: used to suggest a renamed key on collision (ssh-keygen-style prompt)
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

// defaultTxcoSSHKeyBase is the filename for the txco-owned signing key
// that lives next to the user's other SSH keys. Drop-in for
// ssh-add / ssh-agent / any tooling that scans ~/.ssh/.
const defaultTxcoSSHKeyBase = "id_ed25519-txco"

// defaultTxcoSSHKeyPath returns ~/.ssh/id_ed25519-txco.
func defaultTxcoSSHKeyPath() (string, error) {
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home dir: %w", err)
	}
	return filepath.Join(home, ".ssh", defaultTxcoSSHKeyBase), nil
}

// tryDefaultTxcoKey is the no-flag default: either reuse the existing
// ~/.ssh/id_ed25519-txco file (load + return its pubkey) or stage a
// fresh ed25519 keygen for write at that path. No prompts, no
// ssh-agent probing — explicit and predictable.
//
// The fresh-keygen branch produces a key byte-for-byte compatible
// with `ssh-keygen -t ed25519 -f ~/.ssh/id_ed25519-txco`, so once
// PersistFreshKey commits it after a successful chassis enrolment,
// ssh-add / ssh / any SSH tool picks it up like any other key.
//
// `~/.ssh/` is created at mode 0700 only if it doesn't already exist
// — we never chmod a user's pre-existing dir.
func tryDefaultTxcoKey(stderr io.Writer) (*EnrollmentKey, error) {
	path, err := defaultTxcoSSHKeyPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ensure %s: %w", filepath.Dir(path), err)
	}
	if _, err := os.Stat(path); err == nil {
		// Existing file → reuse. Matches --ssh-key semantics.
		return chooseExistingFile(path, stderr)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	// Fresh path: generate in memory; PersistFreshKey writes the
	// OpenSSH PEM + .pub sidecar after the chassis confirms.
	return makeFreshFileEnrollmentKey(path, stderr)
}

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
	switch {
	case c.SSHAgent:
		return chooseAgentKey(stdin, isTTY, stderr)
	case c.SSHKey != "":
		return chooseExistingFile(c.SSHKey, stderr)
	case c.NewKey:
		return generateFreshKey(c.Name, c.Label, stdin, isTTY, stderr)
	}

	// No explicit flag: drop a fresh ed25519 at ~/.ssh/id_ed25519-txco
	// (or reuse it if already there). Never reaches into ssh-agent or
	// the user's other SSH keys without being asked to.
	return tryDefaultTxcoKey(stderr)
}

// chooseAgentKey is the --ssh-agent (explicit) path. The caller
// explicitly asked for an agent key, so we don't second-guess with a
// confirmation prompt — the flag IS the confirmation. Returns
// ErrNoAgent when SSH_AUTH_SOCK is unset or the agent has no Ed25519
// identities, and a hard error for multi-key-on-non-TTY. On TTY with
// multiple keys, prompts the user to pick.
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

// selectAgentKey is the agent-key picker for the --ssh-agent path.
// Returns the single matching agent.Key, or an appropriate error:
//
//   - signer.ErrNoAgent: socket unset / unreachable.
//   - hard error: agent reachable but has zero Ed25519 keys, or
//     multiple Ed25519 keys on non-TTY (the user gets actionable text).
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

// makeFreshFileEnrollmentKey generates a new ed25519 keypair staged
// for write at the given path. The actual disk write happens via
// EnrollmentKey.PersistFreshKey AFTER the chassis confirms enrolment,
// so a failed enrol leaves no orphan files. The resulting on-disk
// artifacts (OpenSSH PEM + .pub sidecar) are byte-for-byte what
// `ssh-keygen -t ed25519 -f <path>` would write — fully usable by any
// other SSH tooling later.
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
