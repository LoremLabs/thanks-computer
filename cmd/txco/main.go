package main

import (
	"os"
	// Embed the IANA time-zone database so &tz / time.LoadLocation resolve
	// zones (e.g. "Asia/Tokyo") even on minimal images with no system
	// zoneinfo (scratch/distroless).
	_ "time/tzdata"

	"github.com/loremlabs/thanks-computer/chassis/app"
)

// Set via -ldflags "-X main.Version=... -X main.CommitId=... -X
// main.BuildTimestamp=... -X main.InstallMethod=..." at build time
// (chassis/Makefile stamps source; .github/workflows/release.yml stamps
// release). Kept in package main so the existing ldflag paths are
// unchanged; the values are handed to app.Run.
//
// InstallMethod is the build origin, not the final install method: the
// release build stamps "release", and the update package refines that at
// runtime (a release binary sitting in a Homebrew Cellar is brew-managed).
// Any unstamped/dev build defaults to "source", which forbids self-update.
var (
	Version        = "0.2.8"
	CommitId       = "dev"
	BuildTimestamp = ""
	InstallMethod  = "source"
)

func main() {
	os.Exit(app.Run(app.BuildInfo{
		Version:        Version,
		CommitId:       CommitId,
		BuildTimestamp: BuildTimestamp,
		InstallMethod:  InstallMethod,
	}))
}
