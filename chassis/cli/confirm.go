package cli

import (
	"io"
	"os"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// confirmMutation guards a mutating workspace/admin command. It prints the
// resolved target and, for a non-local chassis, requires confirmation before
// proceeding (or `--yes`/assumeYes to skip); in a non-interactive shell
// without --yes it fails closed. jsonOut suppresses the interactive prompt so
// machine output stays clean (treated like a pipe → fail-closed unless --yes).
// Returns a non-nil error to abort the command.
//
// Read-only commands don't call this. The heavy lifting (local-chassis
// detection, prompt, fail-closed) lives in auth.ConfirmTarget so the auth
// family can guard with identical behavior.
// confirmMutation guards a mutating command. name is the resolved destination
// label (use resolveTargetLabel so it reflects the profile/target actually
// chosen, not a stale "dev" default), addr the chassis URL, tenant the tenant
// the write lands in ("" to omit). See auth.ConfirmTargetT for the banner +
// prompt semantics.
func confirmMutation(name, addr, tenant string, assumeYes, jsonOut bool, stderr io.Writer) error {
	return auth.ConfirmTargetT(name, addr, tenant, assumeYes, auth.StdinIsTTY() && !jsonOut, os.Stdin, stderr)
}

// confirmMutationTF is the targetFlags-driven convenience over confirmMutation:
// it resolves the destination label, endpoint, and tenant from the workspace +
// flags the same way the command will, so call sites stay one line. Call it
// after the workspace dir is known and before the first write.
func confirmMutationTF(dir string, tf *targetFlags, jsonOut bool, stderr io.Writer) error {
	label := resolveTargetLabel(dir, tf.Target, tf.Addr, tf.Profile)
	ct := resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
	tenant := resolveTenant(tf.Tenant, effectiveProfile(tf.Target, tf.Profile))
	return confirmMutation(label, ct.Addr, tenant, tf.Yes, jsonOut, stderr)
}
