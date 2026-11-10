// Package template is the skeleton of a new lith project: a small guestbook
// with its tests, deploy files and CI, which cmd/lith-new copies into a new
// module. lith's go.mod ignores this directory, so the template compiles only
// where it was copied to.
package template

import (
	"embed"

	"github.com/zweiundeins/lith/datastar"
)

// FS holds the template's files. go.mod.tmpl becomes the project's go.mod;
// this file stays behind. Module zips leave out files in subdirectories of a
// vendor directory, so vendored files sit in vendor/ itself, or a published
// lith would lack them.
//
//go:embed all:*
var FS embed.FS

// DatastarVersion is datastar.Version.
//
// Deprecated: Use datastar.Version.
const DatastarVersion = datastar.Version

// Datastar returns datastar.Datastar(), the build the template serves. An
// app that calls it embeds the build, not FS.
//
// Deprecated: Use datastar.Datastar.
func Datastar() []byte { return datastar.Datastar() }
