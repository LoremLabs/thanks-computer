package cloud

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
)

// runLogout deletes the stored cloud tokens for a profile. With --revoke
// it first makes a best-effort token revocation at the issuer; deletion is
// the guaranteed effect either way.
func runLogout(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("logout", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileFlag := fs.String("profile", "", "cloud profile name")
	revoke := fs.Bool("revoke", false, "best-effort revoke the refresh token at the cloud before deleting")
	dev := fs.Bool("dev", false, "use the local dev cloud for revocation")
	cloudFlag := fs.String("cloud", "", "cloud base URL for revocation (default: the token's stored cloud)")
	insecure := fs.Bool("insecure", false, "skip TLS verification for revocation (local dev cloud only)")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, "\nUsage: txco logout [flags]\n\nDelete stored cloud tokens for the profile. With --revoke, best-effort\nrevoke the refresh token at the cloud first.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	profile := resolveCloudReadProfile(*profileFlag)

	// Load first so we can revoke + name the identity. Absence is fine.
	tok, _ := LoadCloudToken(profile)

	if *revoke && tok != nil && tok.RefreshToken != "" {
		cloudBase := strings.TrimSpace(*cloudFlag)
		if cloudBase == "" {
			switch {
			case *dev:
				cloudBase = devCloudURL
			case tok.CloudURL != "":
				cloudBase = tok.CloudURL
			case tok.Issuer != "":
				cloudBase = tok.Issuer
			default:
				cloudBase = defaultCloudURL
			}
		}
		insecureTLS := *insecure && isLocalCloud(cloudBase)
		hc := newHTTPClient(insecureTLS)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cfg, _ := discover(ctx, hc, cloudBase)
		if err := revokeToken(ctx, hc, cfg, tok.RefreshToken); err != nil {
			fmt.Fprintf(stderr, "logout: revoke failed (continuing to delete local token): %v\n", err)
		}
	}

	existed, derr := DeleteCloudToken(profile)
	if derr != nil {
		auth.PrintCLIErrorf(stderr, "logout: %v", derr)
		return 1
	}
	if !existed {
		fmt.Fprintf(stdout, "Not signed in (profile %q).\n", profile)
		return 0
	}
	who := ""
	if tok != nil && tok.Subject != "" {
		who = " (" + tok.Subject + ")"
	}
	fmt.Fprintf(stdout, "Logged out of cloud%s.\n", who)
	return 0
}
