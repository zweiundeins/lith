// Package migrations holds the board's schema: NNN_name.sql files that
// sqlite.Open applies in order, each once.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
