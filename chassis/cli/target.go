package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/loremlabs/thanks-computer/chassis/cli/auth"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/cli/signer"
)

// workspaceConfig is the parsed shape of `txco.yaml` (or its legacy
// equivalents). The schema deliberately layers — top-level fields hold
// defaults; per-target blocks override only what changes for that
// environment.
//
// Back-compat: a flat config carrying only `addr` / `user` / `pass`
// (the v1 shape) still works. Such a config behaves as if it declared
// a single `dev` target with that chassis URL and basic auth.
type workspaceConfig struct {
	// Stack is an optional default stack name; reserved for future use by
	// commands like `txco init` that take a stack arg today.
	Stack string `yaml:"stack" json:"stack,omitempty"`

	// DefaultTarget names the target picked when --target is omitted.
	// Defaults to "dev" when unset.
	DefaultTarget string `yaml:"target" json:"target,omitempty"`

	Apps       map[string]appConfig       `yaml:"apps" json:"apps,omitempty"`
	Operations map[string]operationConfig `yaml:"operations" json:"operations,omitempty"`
	Targets    map[string]targetConfig    `yaml:"targets" json:"targets,omitempty"`

	// Registry configures package-ref resolution (default registry + namespace
	// for bare `name@ver` refs). PARSED in Phase 1 so a `registry:` block is
	// valid; the resolver that consumes it (bare/namespaced ref → concrete OCI
	// ref) lands with the oci:// source in Phase 2. See
	// docs/txco-oci-packages.md §6 — config-driven default, no hardwired host.
	Registry registryConfig `yaml:"registry" json:"registry,omitempty"`

	// Trust lists the public keys this workspace trusts for package signature
	// verification (`txco install --require-signature`). No key is trusted by
	// default — the blessed namespace is metadata, never runtime-special.
	Trust trustConfig `yaml:"trust" json:"trust,omitempty"`

	// Legacy flat fields. When neither `target` nor any `targets:` block
	// exists, these populate a synthesized "dev" target.
	Addr string `yaml:"addr" json:"addr,omitempty"`
	User string `yaml:"user" json:"user,omitempty"`
	Pass string `yaml:"pass" json:"pass,omitempty"`
}

// registryConfig is the txco.yaml `registry:` block. Phase 1 only parses it.
type registryConfig struct {
	Default          string            `yaml:"default" json:"default,omitempty"`                   // e.g. registry.thanks.computer
	DefaultNamespace string            `yaml:"defaultNamespace" json:"defaultNamespace,omitempty"` // e.g. txco
	Aliases          map[string]string `yaml:"aliases" json:"aliases,omitempty"`
}

// trustConfig is the txco.yaml `trust:` block — public keys trusted to sign
// packages, optionally scoped to a registry host.
type trustConfig struct {
	Keys []trustKey `yaml:"keys" json:"keys,omitempty"`
}

type trustKey struct {
	Name     string `yaml:"name" json:"name,omitempty"`
	Pubkey   string `yaml:"pubkey" json:"pubkey,omitempty"`     // ssh-ed25519 line, .pub path, or base64 raw
	Registry string `yaml:"registry" json:"registry,omitempty"` // optional host scope
}

type appConfig struct {
	Path   string `yaml:"path" json:"path"`
	Start  string `yaml:"start" json:"start"`
	Health string `yaml:"health" json:"health"`
}

type operationConfig struct {
	// URL routes op://NAME to a remote worker (EXEC "https://…"). Local
	// sandboxed computes are no longer declared here — they're a colocated
	// <name>.js next to the resonator, discovered at apply time.
	URL string `yaml:"url" json:"url"`
}

type targetConfig struct {
	Chassis    string                     `yaml:"chassis" json:"chassis"`
	User       string                     `yaml:"user" json:"user,omitempty"`
	Pass       string                     `yaml:"pass" json:"pass,omitempty"`
	Mock       string                     `yaml:"mock" json:"mock,omitempty"` // "allow" | "deny"; default "allow"
	Operations map[string]operationConfig `yaml:"operations" json:"operations,omitempty"`
}

// ResolvedTarget is what apply / diff / dev consume. It's flat by design:
// no operation merging or auth lookup happens past this point.
type ResolvedTarget struct {
	Name    string
	Chassis string
	// ChassisExplicit is true when Chassis came from real config
	// (txco.yaml target / legacy addr), false when it's the
	// synthesized http://localhost:8081 fallback. Lets resolveTarget
	// prefer the signing profile's bound chassis_url over a blind
	// localhost default, without overriding an explicit config.
	ChassisExplicit bool
	User            string
	Pass            string
	Mock            string // "allow" | "deny"
	Operations      map[string]operationConfig
}

// AsClientTarget reduces a ResolvedTarget to the subset the admin HTTP
// client needs (URL + basic auth). Kept separate so the dev/apply code
// paths can keep all the operation map / mock policy bits without
// leaking them into the client package.
func (t ResolvedTarget) AsClientTarget() client.Target {
	return client.Target{
		Addr: t.Chassis,
		User: t.User,
		Pass: t.Pass,
	}
}

// resolveTarget builds a client.Target by layering, highest precedence first:
//  1. explicit --addr/--user/--pass flags (as passed in)
//  2. TXCO_ADMIN_ADDR / TXCO_ADMIN_USER / TXCO_ADMIN_PASS env vars
//  3. selected target from the workspace config file
//  4. the resolved profile's bound chassis_url — an explicit --profile if
//     passed, else the active profile (auth.ProfileChassisURL)
//  5. http://localhost:8081 with no auth
//
// dir is the workspace root the user passed to apply/diff/dev (so the
// config is per-workspace, not per-cwd).
//
// targetName, when non-empty, selects which target to read from the config.
// When empty, falls back to the config's default target (or "dev").
//
// profile, when non-empty, is the explicit --profile flag value. Empty
// means "use the active profile" (per auth.ResolveProfile's chain).
// Signing credentials are attached by loadSigner — when the resolved
// profile has a key the resulting Target.Auth signs every outgoing
// request and basic-auth is ignored.
// looksLikeURL reports whether a --target value is a raw admin endpoint rather
// than a name. A scheme ("http://…") or a host:port ("localhost:8081") has a
// ":"; a bare txco.yaml target / profile name ("staging", "cloud") does not.
func looksLikeURL(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && strings.Contains(v, ":")
}

func resolveTarget(dir, targetName, addr, user, pass, profile string) client.Target {
	// --target may be a raw admin URL rather than a NAME — treat it like an
	// explicit --addr so `txco apply --target http://host:8081` works without a
	// txco.yaml. A bare name ("staging"/"cloud") falls through to be resolved
	// below as a txco.yaml target, else a profile.
	if addr == "" && looksLikeURL(targetName) {
		addr = targetName
		targetName = ""
	}

	t := resolveFullTarget(dir, targetName)
	target := t.AsClientTarget()

	if v := os.Getenv("TXCO_ADMIN_ADDR"); v != "" {
		target.Addr = v
	}
	if v := os.Getenv("TXCO_ADMIN_USER"); v != "" {
		target.User = v
	}
	if v := os.Getenv("TXCO_ADMIN_PASS"); v != "" {
		target.Pass = v
	}

	explicitAddr := addr != "" || os.Getenv("TXCO_ADMIN_ADDR") != ""
	if addr != "" {
		target.Addr = addr
	}
	if user != "" {
		target.User = user
	}
	if pass != "" {
		target.Pass = pass
	}

	// Endpoint fallback: when no endpoint was given (no --addr/env, no
	// txco.yaml target), use the RESOLVED signing profile's own bound
	// chassis_url — an explicit --profile if one was passed, otherwise the
	// active profile. This mirrors ResolveTenant, which already follows the
	// active profile, so after `txco login` a bare `txco status`/`apply`
	// targets the cloud the active profile is bound to instead of a blind
	// localhost (the asymmetry where the tenant followed the active profile
	// but the address didn't). A workspace txco.yaml target (ChassisExplicit)
	// and an explicit --addr/TXCO_ADMIN_ADDR still win — so local dev inside a
	// workspace is unaffected: its configured target overrides the active
	// profile.
	//   1. --target <name> as a PROFILE — a named chassis carries its own
	//      chassis_url AND signing key (the git-remote feel: `txco apply cloud`).
	//      Only when --profile wasn't given separately.
	//   2. else the --profile / active profile's chassis_url.
	signProfile := profile
	if !explicitAddr && !t.ChassisExplicit {
		if u := auth.ProfileChassisURL(targetName); targetName != "" && profile == "" && u != "" {
			target.Addr = u
			signProfile = targetName
		} else if u := auth.ProfileChassisURL(profile); u != "" {
			target.Addr = u
		}
	}

	if s := loadSigner(signProfile); s != nil {
		target.Auth = s
	}

	return target
}

// resolveTenant is a thin pass-through to auth.ResolveTenant. The
// upper cli package keeps the verbose flag wiring here while the
// logic lives in cli/auth where its dependencies (MetaPath,
// ResolveProfile, LoadMeta) already live without an import cycle.
func resolveTenant(flag, profileFlag string) string {
	return auth.ResolveTenant(flag, profileFlag)
}

// loadSigner returns a signed-request backend when one is configured,
// or nil to fall through to basic / open. Thin wrapper around
// auth.LoadSignerForActiveProfile (and LoadSignerForMetaPath for the
// CI-style explicit-path escape hatch). The heavy lifting lives in
// cli/auth so the auth-side commands (whoami, etc.) can use the
// same code path.
//
// Selection precedence (highest first):
//  1. TXCO_PRIVATE_KEY_PATH (file path) + sibling .meta.json — both
//     pinned, used as-is. Survives across profile churn; ideal for
//     CI that wants byte-exact control over which key gets used.
//  2. profileFlag (the --profile CLI flag), if set.
//  3. TXCO_PROFILE env var.
//  4. $TXCO_HOME/active file contents.
//  5. "local" (the historical default).
//
// Profile resolution = #2 → #3 → #4 → #5 (handled inside
// auth.LoadSignerForActiveProfile). If the resolved profile is
// "none" (logout sentinel), nil is returned and the request is
// sent unsigned.
//
// Errors are swallowed (returning nil) on this path so basic-auth
// and open-mode chassis remain usable when no key is configured.
// The first signed request against a chassis that REQUIRES signing
// will fail with a clear chassis-side 401.
func loadSigner(profileFlag string) signer.Signer {
	if explicit := os.Getenv("TXCO_PRIVATE_KEY_PATH"); explicit != "" {
		s, err := auth.LoadSignerForMetaPath(explicit + ".meta.json")
		if err != nil {
			return nil
		}
		return s
	}
	s, err := auth.LoadSignerForActiveProfile(profileFlag)
	if err != nil {
		return nil
	}
	return s
}

// resolveFullTarget returns the full ResolvedTarget — chassis URL, auth,
// merged operations map, and mock policy — for the given workspace and
// target name. Falls back to a synthesized localhost target if no config
// is present.
func resolveFullTarget(dir, targetName string) ResolvedTarget {
	cfg := loadWorkspaceConfig(dir)
	return resolveFullTargetFromConfig(cfg, targetName)
}

// resolveFullTargetFromConfig is the testable core: given a parsed config
// (possibly nil), pick the named target and merge operations.
func resolveFullTargetFromConfig(cfg *workspaceConfig, targetName string) ResolvedTarget {
	out := ResolvedTarget{
		Name:    "dev",
		Chassis: "http://localhost:8081",
		Mock:    "allow",
	}

	if cfg == nil {
		return out
	}

	// Legacy flat config: synthesize a single "dev" target from
	// addr/user/pass when no targets block exists.
	if len(cfg.Targets) == 0 {
		if cfg.Addr != "" {
			out.Chassis = cfg.Addr
			out.ChassisExplicit = true
		}
		out.User = cfg.User
		out.Pass = cfg.Pass
		out.Operations = cloneOps(cfg.Operations)
		return out
	}

	// Resolve which target to pick.
	pick := targetName
	if pick == "" {
		pick = cfg.DefaultTarget
	}
	if pick == "" {
		pick = "dev"
	}
	out.Name = pick

	tcfg, ok := cfg.Targets[pick]
	if !ok {
		// Selected target doesn't exist — fall through with localhost
		// defaults. Apply/dev surface this as a clearer error before
		// reaching this point; this keeps resolveFullTarget total.
		return out
	}

	if tcfg.Chassis != "" {
		out.Chassis = tcfg.Chassis
		out.ChassisExplicit = true
	}
	if tcfg.User != "" {
		out.User = tcfg.User
	}
	if tcfg.Pass != "" {
		out.Pass = tcfg.Pass
	}
	if tcfg.Mock != "" {
		out.Mock = tcfg.Mock
	}

	// Merge ops: target-level override wins; top-level fills in the rest.
	out.Operations = cloneOps(cfg.Operations)
	if out.Operations == nil {
		out.Operations = make(map[string]operationConfig)
	}
	for name, op := range tcfg.Operations {
		out.Operations[name] = op
	}

	return out
}

// loadWorkspaceConfig walks the canonical config-file lookup order and
// returns the first successful parse. Top-level `txco.yaml` is preferred;
// the `.txco/target.*` paths are kept as legacy fallbacks.
func loadWorkspaceConfig(dir string) *workspaceConfig {
	if dir == "" {
		return nil
	}
	candidates := []string{
		filepath.Join(dir, "txco.yaml"),
		filepath.Join(dir, "txco.yml"),
		filepath.Join(dir, "txco.json"),
		filepath.Join(dir, ".txco", "target.yaml"),
		filepath.Join(dir, ".txco", "target.yml"),
		filepath.Join(dir, ".txco", "target.json"),
	}
	for _, full := range candidates {
		t, err := parseConfigFile(full)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			// Parse error: silently fall through. CLI is best-effort here;
			// flags/env can still drive the run.
			continue
		}
		return t
	}
	return nil
}

func parseConfigFile(path string) (*workspaceConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t workspaceConfig
	switch ext := filepath.Ext(path); ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	case ".json":
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	default:
		return nil, fmt.Errorf("unsupported config file extension %q", ext)
	}
	return &t, nil
}

func cloneOps(in map[string]operationConfig) map[string]operationConfig {
	if in == nil {
		return nil
	}
	out := make(map[string]operationConfig, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
