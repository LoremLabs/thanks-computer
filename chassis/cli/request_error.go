package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// is403 reports whether err is a 403 Forbidden from the chassis.
func is403(err error) bool {
	var he *client.HTTPError
	return errors.As(err, &he) && he.StatusCode == http.StatusForbidden
}

// forbiddenIdentity asks the chassis who the current signing profile is, so a
// 403 can name the caller — the usual cause is that the active profile isn't
// super_admin (or lacks the capability). It signs with the SAME target, so the
// returned identity is exactly the one that was just denied. Best-effort:
// returns "" if whoami itself fails (no signer, key not enrolled, etc.).
func forbiddenIdentity(t client.Target) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	who, err := client.New(t).Whoami(ctx)
	if err != nil || who == nil {
		return ""
	}
	id := who.ActorID
	if id == "" {
		id = "source=" + who.Source
	}
	bits := []string{id}
	if who.Label != "" {
		bits = append(bits, fmt.Sprintf("label %q", who.Label))
	}
	bits = append(bits, fmt.Sprintf("super_admin=%t", who.SuperAdmin))
	return "signed in as " + strings.Join(bits, ", ")
}

// resolveProfileName returns the local signing profile that was used for the
// request — the explicit --profile if set, else TXCO_PROFILE, else the active
// profile. Empty when logged out / none; a sentinel when a raw key path
// overrides profile resolution.
func resolveProfileName(profileFlag string) string {
	if os.Getenv("TXCO_PRIVATE_KEY_PATH") != "" {
		return "(TXCO_PRIVATE_KEY_PATH)"
	}
	name, err := auth.ResolveProfile(profileFlag)
	if err != nil || name == "" || name == auth.ActiveNone {
		return ""
	}
	return name
}

// requestErrorMessage formats a failed chassis request for auth.PrintCLIError.
// On a 403 it names both the LOCAL signing profile (what the user picks with
// --profile / the active profile) and the chassis's view of that identity (via
// whoami), so the cause — usually an under-privileged profile — is obvious.
func requestErrorMessage(prefix string, t client.Target, profileFlag string, err error) string {
	msg := fmt.Sprintf("%s: %v", prefix, err)
	if !is403(err) {
		return msg
	}
	name := resolveProfileName(profileFlag)
	who := forbiddenIdentity(t)
	var hint string
	switch {
	case name != "" && who != "":
		hint = fmt.Sprintf("active profile %q — %s", name, who)
	case name != "":
		hint = fmt.Sprintf("active profile %q", name)
	case who != "":
		hint = who
	}
	if hint != "" {
		msg += "\n\n\t" + hint +
			"\n\t(use --profile <name> or switch the active profile if this needs more privilege)"
	}
	return msg
}
