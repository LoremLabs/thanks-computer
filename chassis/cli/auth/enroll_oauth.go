package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// OAuthEnrollOptions drives OAuthEnroll. The cloud package resolves the full
// enroll endpoint URL and supplies the verified id_token; this routine owns the
// ed25519 key, the tenant-slug round-trip, and the chassis profile write.
type OAuthEnrollOptions struct {
	EndpointURL string // full URL, e.g. https://admin.thanks.computer/auth/oauth/enroll
	IDToken     string // bearer secret — never logged
	Profile     string // CLI profile name to write + activate (default "cloud")
	Label       string
	TenantSlug  string // explicit --tenant; empty means "let the server suggest"
	AssumeYes   bool   // --yes: accept the server's suggestion without prompting

	// Key selection — mirrors `txco auth accept`'s steering. Zero values
	// take the default: ~/.ssh/id_ed25519-txco (load if exists, else
	// fresh keygen). Explicit flags override.
	SSHAgent bool
	SSHKey   string
	NewKey   bool

	Stdin  io.Reader
	Stderr io.Writer
}

// OAuthEnrollResult is the data a caller needs to print a summary.
type OAuthEnrollResult struct {
	ActorID      string
	KeyID        string
	TenantSlug   string
	ChassisURL   string
	Capabilities []string
	Fingerprint  string
	KeyPath      string
	MetaPath     string
	Profile      string
}

// slugConflictCodes are the 409 error codes that carry a suggested_tenant_slug
// and invite a resubmit.
var slugConflictCodes = map[string]bool{
	"tenant_slug_required": true,
	"tenant_slug_taken":    true,
	"tenant_slug_invalid":  true,
}

// OAuthEnroll resolves an ed25519 key, exchanges the id_token + public key at
// the cloud enroll endpoint, writes the chassis profile, and makes it active.
// On a first enroll the server asks the user to name their tenant: a TTY
// prompts; a non-interactive run (or --yes) takes the server's suggestion; an
// explicit --tenant that conflicts is a hard error (never silently swapped).
func OAuthEnroll(opts OAuthEnrollOptions) (*OAuthEnrollResult, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	profile := strings.TrimSpace(opts.Profile)
	if profile == "" {
		profile = "cloud"
	}
	isTTY := term.IsTerminal(int(os.Stdin.Fd()))

	ek, err := resolveEnrollmentKey(EnrollmentChoices{
		SSHAgent: opts.SSHAgent,
		SSHKey:   opts.SSHKey,
		NewKey:   opts.NewKey,
		Name:     profile,
		Label:    opts.Label,
	}, stdin, isTTY, stderr)
	if err != nil {
		return nil, err
	}

	effLabel := opts.Label
	if effLabel == "" {
		effLabel = ek.CommentSuggestion
	}

	cl := client.New(client.Target{Addr: opts.EndpointURL})
	ctx := context.Background()

	slug := strings.TrimSpace(opts.TenantSlug)
	explicitSlug := slug != ""

	var resp *client.OAuthEnrollResponse
	for attempt := 0; ; attempt++ {
		r, err := cl.OAuthEnroll(ctx, opts.EndpointURL, client.OAuthEnrollRequest{
			IDToken:    opts.IDToken,
			PublicKey:  PublicKeyB64(ek.PublicKey),
			Label:      effLabel,
			Profile:    profile,
			TenantSlug: slug,
		})
		if err == nil {
			resp = r
			break
		}

		var he *client.HTTPError
		if errors.As(err, &he) && he.StatusCode == http.StatusConflict && slugConflictCodes[he.Code] {
			suggested, _ := he.Detail["suggested_tenant_slug"].(string)

			// An explicit --tenant that conflicts is a hard error — don't
			// substitute a different name than the user asked for.
			if explicitSlug {
				ek.CleanupOnFailure()
				return nil, fmt.Errorf("tenant slug %q is not available (%s) — choose another with --tenant", slug, he.Code)
			}
			if attempt >= 25 {
				ek.CleanupOnFailure()
				return nil, fmt.Errorf("could not settle on an available tenant slug after %d attempts", attempt)
			}
			if isTTY && !opts.AssumeYes {
				chosen, _, perr := promptLine(stderr, "Name your cloud space", suggested)
				if perr != nil {
					ek.CleanupOnFailure()
					return nil, perr
				}
				slug = strings.TrimSpace(chosen)
				if slug == "" {
					slug = suggested
				}
			} else {
				if suggested == "" {
					ek.CleanupOnFailure()
					return nil, fmt.Errorf("enrollment requires a tenant slug and the server offered no suggestion; pass --tenant")
				}
				slug = suggested
			}
			continue
		}

		ek.CleanupOnFailure()
		return nil, err
	}

	if err := ek.PersistFreshKey(effLabel); err != nil {
		return nil, fmt.Errorf("persist key: %w", err)
	}

	metaName := profile
	if ek.MetaName() != "" {
		metaName = ek.MetaName()
	}
	metaPath, err := MetaPath(metaName)
	if err != nil {
		return nil, err
	}
	if err := SaveMeta(metaPath, Meta{
		ActorID:       resp.ActorID,
		KeyID:         resp.KeyID,
		ChassisURL:    resp.ChassisURL,
		Label:         effLabel,
		EnrolledAt:    time.Now().UTC(),
		KeySource:     ek.KeySource,
		PublicKeyB64:  PublicKeyB64(ek.PublicKey),
		KeyPath:       ek.KeyPath,
		DefaultTenant: resp.TenantSlug,
	}); err != nil {
		return nil, err
	}
	if err := WriteActiveProfile(metaName); err != nil {
		return nil, err
	}

	return &OAuthEnrollResult{
		ActorID:      resp.ActorID,
		KeyID:        resp.KeyID,
		TenantSlug:   resp.TenantSlug,
		ChassisURL:   resp.ChassisURL,
		Capabilities: resp.Capabilities,
		Fingerprint:  ek.Fingerprint,
		KeyPath:      ek.KeyPath,
		MetaPath:     metaPath,
		Profile:      metaName,
	}, nil
}
