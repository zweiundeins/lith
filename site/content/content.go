// Package content holds the site's Markdown: the docs, one directory per
// section (internal/docs reads them). The package reference and the rows
// of the charts drawn from lith's benchmarks are generated (cmd/docgen).
package content

//go:generate go run ../cmd/docgen

import (
	"embed"
	"io/fs"
)

//go:embed docs
var files embed.FS

// Docs is the docs directory.
var Docs, _ = fs.Sub(files, "docs")
