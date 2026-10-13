// Package migrations holds the site's own schema, the "site" scope: tables
// every demo shares. Each demo keeps its migrations in its own package
// (internal/demos/<slug>/migrations), numbered from 001 within the demo.
// Add a file for every change; never edit one that has shipped.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
