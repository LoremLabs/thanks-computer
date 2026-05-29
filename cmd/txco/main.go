package main

import (
	"os"

	"github.com/loremlabs/thanks-computer/chassis/app"
)

// Set via -ldflags "-X main.Version=... -X main.CommitId=... -X
// main.BuildTimestamp=..." at build time (chassis/Makefile). Kept in
// package main so the existing ldflag paths are unchanged; the values
// are handed to app.Run.
var (
	Version        = "0.2.1"
	CommitId       = "dev"
	BuildTimestamp = ""
)

func main() {
	os.Exit(app.Run(app.BuildInfo{
		Version:        Version,
		CommitId:       CommitId,
		BuildTimestamp: BuildTimestamp,
	}))
}
