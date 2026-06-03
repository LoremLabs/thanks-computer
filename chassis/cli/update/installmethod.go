// Package update implements `txco update check` and `txco upgrade`: it
// discovers the latest release from GitHub, compares versions, detects how
// the running binary was installed, and (for self-managed installs)
// downloads + verifies + atomically replaces the binary.
//
// The package is pure with respect to the CLI surface — it takes the build
// version / install-method origin as plain strings so it never imports
// chassis/cli (which would be a cycle). The CLI layer (chassis/cli/
// update_cmd.go) reads cli.Build and calls in.
package update

import (
	"os"
	"path/filepath"
	"strings"
)

// Method is the resolved install method plus its self-update policy.
type Method struct {
	// Name is the effective method: "source" | "manual" | "homebrew" |
	// "unknown". ("manual" covers any self-managed release binary — a direct
	// download or the curl installer; they are byte-identical.)
	Name string
	// SelfUpdate reports whether txco may replace its own binary in place.
	SelfUpdate bool
}

// Resolve maps the build origin (ldflag main.InstallMethod, mirrored to
// cli.Build.InstallMethod) plus the running executable's path to the
// effective install method.
//
// The released binary is a single artifact per os/arch, shared by the
// curl/manual download AND Homebrew (the formula installs the prebuilt
// binary — no compile step), so the ldflag alone can't distinguish them: a
// "release" binary under a Homebrew prefix is brew-managed and must
// delegate; anywhere else it is self-managed. "source" (Makefile / unstamped
// dev builds) and anything unrecognized never self-update.
//
// exePath should be the symlink-resolved path to the running binary;
// ResolveCurrent supplies it for the live process.
func Resolve(origin, exePath string) Method {
	switch origin {
	case "release":
		if isHomebrewPath(exePath) {
			return Method{Name: "homebrew", SelfUpdate: false}
		}
		return Method{Name: "manual", SelfUpdate: true}
	case "source", "":
		return Method{Name: "source", SelfUpdate: false}
	default:
		// Future explicit origins (apt/nix/…) land here: unknown ⇒ never
		// self-update, which is the safe default (delegate or no-op).
		return Method{Name: "unknown", SelfUpdate: false}
	}
}

// ResolveCurrent resolves the install method for the running process: the
// build origin from cli.Build.InstallMethod plus the symlink-resolved path
// of the executable.
func ResolveCurrent(origin string) Method {
	exe, err := os.Executable()
	if err == nil {
		if real, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = real
		}
	}
	return Resolve(origin, exe)
}

// isHomebrewPath reports whether p looks like it lives under a Homebrew
// installation. Homebrew symlinks <prefix>/bin/txco into
// <prefix>/Cellar/txco/<version>/bin/txco, so the symlink-resolved path
// contains a "/Cellar/" segment — the primary, prefix-agnostic signal
// (covers /opt/homebrew on arm64 and /usr/local on intel). $HOMEBREW_CELLAR
// / $HOMEBREW_PREFIX are honored as a fallback.
func isHomebrewPath(p string) bool {
	if p == "" {
		return false
	}
	if strings.Contains(p, "/Cellar/") {
		return true
	}
	for _, env := range []string{os.Getenv("HOMEBREW_CELLAR"), os.Getenv("HOMEBREW_PREFIX")} {
		if env != "" && strings.HasPrefix(p, strings.TrimRight(env, "/")+"/") {
			return true
		}
	}
	return false
}
