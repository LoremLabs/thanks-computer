package cloud

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
)

// CloudToken is the persisted cloud session for one profile, stored as a
// 0600 JSON file at $TXCO_HOME/cloud/<profile>.json. The OAuth token
// represents the signed-in user/account — distinct from the ed25519 keys
// in $TXCO_HOME/keys, which carry chassis admin authority.
type CloudToken struct {
	// Kind is a seam for the future cloud|chassis profile-kind split; today
	// every file under cloud/ is a cloud token. Not yet acted on.
	Kind         string    `json:"kind,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	Expiry       time.Time `json:"expiry"` // absolute = obtained_at + expires_in
	ObtainedAt   time.Time `json:"obtained_at"`
	Subject      string    `json:"subject"` // e.g. email:matt@example.com
	Email        string    `json:"email,omitempty"`
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"client_id"`
	CloudURL     string    `json:"cloud_url,omitempty"`
}

// Expired reports whether the access token is at/over its expiry, applying
// a small negative skew. A zero Expiry is treated as not-expired (unknown
// lifetime). The absolute Expiry (stored at login as obtained_at +
// expires_in) means a paused laptop can't misjudge a relative TTL.
//
// Refreshing an expired token (grant_type=refresh_token, using
// RefreshToken) is a fast-follow; this is the hook for it.
func (t *CloudToken) Expired(now time.Time) bool {
	if t.Expiry.IsZero() {
		return false
	}
	return now.After(t.Expiry.Add(-30 * time.Second))
}

// cloudDir returns $TXCO_HOME/cloud, created 0700.
func cloudDir() (string, error) {
	home, err := auth.HomePath()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "cloud")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("cloud: mkdir %q: %w", dir, err)
	}
	return dir, nil
}

// validProfileName guards against path traversal / escaping the cloud dir.
func validProfileName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

// tokenPath returns the token file path for a profile.
func tokenPath(profile string) (string, error) {
	if !validProfileName(profile) {
		return "", fmt.Errorf("invalid profile name %q", profile)
	}
	dir, err := cloudDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, profile+".json"), nil
}

// SaveCloudToken writes the token atomically (temp + rename) with 0600,
// matching the discipline used for ed25519 keys.
func SaveCloudToken(profile string, t CloudToken) error {
	path, err := tokenPath(profile)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".token-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// LoadCloudToken reads a profile's token file. The returned error wraps
// os.ErrNotExist when the file is absent (use errors.Is).
func LoadCloudToken(profile string) (*CloudToken, error) {
	path, err := tokenPath(profile)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err // os.ErrNotExist preserved
	}
	var t CloudToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("cloud: parse %s: %w", path, err)
	}
	return &t, nil
}

// cloudTokenExists reports whether a stored cloud token exists for the profile.
func cloudTokenExists(profile string) bool {
	path, err := tokenPath(profile)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// soleCloudProfile returns the single stored cloud profile's name when exactly
// one token file exists, so read commands can pick it without a flag. Returns
// ok=false for zero or multiple tokens.
func soleCloudProfile() (string, bool) {
	dir, err := cloudDir()
	if err != nil {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	if len(names) == 1 {
		return names[0], true
	}
	return "", false
}

// DeleteCloudToken removes a profile's token file. A missing file is not
// an error; the bool reports whether a file existed.
func DeleteCloudToken(profile string) (existed bool, err error) {
	path, err := tokenPath(profile)
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
