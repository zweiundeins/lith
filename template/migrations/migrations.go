// Package migrations holds the schema: NNN_name.sql files that sqlite.Open
// applies in order. Add a file for every change; never edit one that has
// shipped.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
