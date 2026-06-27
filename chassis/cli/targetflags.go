package cli

import (
	"fmt"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// targetFlags are the six chassis-target flags every workspace command shares:
// which txco.yaml target to use, endpoint/credential overrides, the signing
// profile, and the tenant. Registered once via bindTargetFlags so the names and
// help text stay identical across commands instead of drifting per copy-paste.
type targetFlags struct {
	Target  string
	Addr    string
	User    string
	Pass    string
	Profile string
	Tenant  string
	// Yes skips the confirmation prompt a mutating command shows before it
	// writes to a NON-local chassis (see confirmMutation / auth.ConfirmTarget).
	// Harmless on read-only commands (diff/status) that never confirm.
	Yes bool
}

// bindTargetFlags registers the standard chassis-target flags on fs and returns
// the struct their values land in (after fs.Parse). Consume them via the
// existing resolveTarget(dir, tf.Target, tf.Addr, tf.User, tf.Pass, tf.Profile)
// and resolveTenant(tf.Tenant, tf.Profile).
//
// Usage strings deliberately avoid back-quotes: pflag treats a back-quoted word
// as the flag's value placeholder, which would mangle `--target string` into
// `--target target`.
func bindTargetFlags(fs *pflag.FlagSet) *targetFlags {
	tf := &targetFlags{}
	fs.StringVar(&tf.Target, "target", "", "target name from txco.yaml (default: the config's target, or dev)")
	fs.StringVar(&tf.Addr, "addr", "", "chassis admin endpoint (overrides the target's chassis URL)")
	fs.StringVar(&tf.User, "user", "", "basic auth user (overrides the target's user)")
	fs.StringVar(&tf.Pass, "pass", "", "basic auth password (overrides the target's pass)")
	fs.StringVar(&tf.Profile, "profile", "", fmt.Sprintf("signing profile (TXCO_PROFILE, then %s/active, then \"local\")", auth.HomePathPretty()))
	fs.StringVar(&tf.Tenant, "tenant", "", "tenant slug (TXCO_TENANT, then meta's default_tenant, then \"default\")")
	fs.BoolVar(&tf.Yes, "yes", false, "skip the confirmation prompt before modifying a non-local chassis")
	return tf
}

// workspaceDir resolves the workspace directory for a command: the explicit
// <dir> arg if given (else cwd), then a git-style walk up to the nearest
// ancestor containing OPS/. This lets every workspace command run from a
// subdirectory, the way `apply`/`push`/`install` already do.
func workspaceDir(dirArg string) (string, error) {
	dir, err := resolveDir(dirArg)
	if err != nil {
		return "", err
	}
	if root := findWorkspaceRoot(dir); root != "" && root != dir {
		dir = root
	}
	return dir, nil
}
