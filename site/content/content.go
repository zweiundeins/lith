// Package content holds the site's Markdown: the docs, one directory per
// section (internal/docs reads them).
package content

import (
	"embed"
	"io/fs"
)

//go:embed docs
var files embed.FS

// Docs is the docs directory.
var Docs, _ = fs.Sub(files, "docs")
