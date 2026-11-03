// Package template is the skeleton of a new lith project: a small guestbook
// with its tests, deploy files and CI, which cmd/lith-new copies into a new
// module. lith's go.mod ignores this directory, so the template compiles only
// where it was copied to.
package template

import (
	"embed"
	"io/fs"
)

// FS holds the template's files. go.mod.tmpl becomes the project's go.mod;
// this file stays behind. Module zips leave out files in subdirectories of a
// vendor directory, so vendored files sit in vendor/ itself, or a published
// lith would lack them.
//
//go:embed all:*
var FS embed.FS

// DatastarVersion names the build that Datastar returns: Datastar v1.0.4
// with Rocket and the patches in Starbase's patches/rocket, as Starbase's
// commit c6a6589 vendors it. VERSION beside the file has its integrity hash.
const DatastarVersion = "1.0.4+starbase.c6a6589"

// Datastar returns the Datastar build the template vendors, for an app that
// serves the same file without naming a path inside FS:
//
//	static.Add("vendor/datastar-rocket.js", template.Datastar())
//	url := static.URL("vendor/datastar-rocket.js")
//
// lith's newest-frame.js is live's: live.Scripts and live.NewestFrame.
func Datastar() []byte {
	b, err := fs.ReadFile(FS, datastarPath)
	if err != nil {
		panic(err) // lith's tests read it, so a release has it
	}
	return b
}

const datastarPath = "internal/web/static/vendor/datastar-rocket.js"
