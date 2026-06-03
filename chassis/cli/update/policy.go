package update

// CanSelfUpdate reports whether the running binary (build origin =
// cli.Build.InstallMethod) may replace itself in place.
func CanSelfUpdate(origin string) bool {
	return ResolveCurrent(origin).SelfUpdate
}

// UpgradeGuidance returns the human instruction for upgrading a binary that
// txco must NOT self-update. Returns "" for self-managed installs (the
// caller performs the in-place update instead).
func UpgradeGuidance(m Method) string {
	switch m.Name {
	case "homebrew":
		return "Installed via Homebrew. Upgrade with:\n\n  brew update\n  brew upgrade txco"
	case "source":
		return "This binary was built from source — rebuild from the repo (make build / make install) to upgrade."
	case "manual":
		return "" // self-managed: caller self-updates
	default:
		return "Can't determine how this binary was installed. Reinstall from https://github.com/loremlabs/thanks-computer/releases"
	}
}
