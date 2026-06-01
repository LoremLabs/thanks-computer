package cloud

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// runWhoami prints the current cloud identity from the stored token file.
// Local read-back only — no network call (a --refresh that re-validates
// against userinfo is a fast-follow).
func runWhoami(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("cloud whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileFlag := fs.String("profile", "", "cloud profile name")
	asJSON := fs.Bool("json", false, "print the identity as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	profile, err := auth.ResolveProfile(*profileFlag)
	if err != nil {
		auth.PrintCLIErrorf(stderr, "cloud whoami: %v", err)
		return 1
	}
	if profile == "" || profile == auth.ActiveNone {
		profile = "cloud"
	}

	tok, err := LoadCloudToken(profile)
	if err != nil {
		auth.PrintCLIError(stderr, "not signed in to cloud (run `txco login`)")
		return 1
	}

	expired := tok.Expired(time.Now())
	if *asJSON {
		out := struct {
			Profile string    `json:"profile"`
			Subject string    `json:"subject"`
			Email   string    `json:"email,omitempty"`
			Issuer  string    `json:"issuer"`
			Expiry  time.Time `json:"expiry"`
			Expired bool      `json:"expired"`
		}{profile, tok.Subject, tok.Email, tok.Issuer, tok.Expiry, expired}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}

	fmt.Fprintf(stdout, "profile:  %s\n", profile)
	fmt.Fprintf(stdout, "subject:  %s\n", tok.Subject)
	if tok.Email != "" {
		fmt.Fprintf(stdout, "email:    %s\n", tok.Email)
	}
	fmt.Fprintf(stdout, "issuer:   %s\n", tok.Issuer)
	if !tok.Expiry.IsZero() {
		status := ""
		if expired {
			status = " (expired)"
		}
		fmt.Fprintf(stdout, "expires:  %s%s\n", tok.Expiry.Format(time.RFC3339), status)
	}
	return 0
}
